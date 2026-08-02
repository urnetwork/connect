package connect

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// newWarmTestDohServer starts a local h2 DoH server answering every A query
// with 127.0.0.1, returning its /dns-query url and client tls config.
func newWarmTestDohServer(t *testing.T) (*httptest.Server, string, *tls.Config) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var msg dnsmessage.Message
		if err := msg.Unpack(wire); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		msg.Response = true
		msg.Authoritative = true
		if len(msg.Questions) == 1 && msg.Questions[0].Type == dnsmessage.TypeA {
			msg.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{
					Name:  msg.Questions[0].Name,
					Type:  dnsmessage.TypeA,
					Class: dnsmessage.ClassINET,
					TTL:   60,
				},
				Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}},
			}}
		}
		out, err := msg.Pack()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(out)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	tlsConfig := server.Client().Transport.(*http.Transport).TLSClientConfig
	return server, server.URL + "/dns-query", tlsConfig
}

// TestDohClientSessionCache pins the TLS-resumption plumbing: every DoH http
// client carries a ClientSessionCache (TLS 1.3 PSK resumption on re-dial) and
// the long idle timeout, and a caller-provided pinned tls config is cloned,
// not mutated.
func TestDohClientSessionCache(t *testing.T) {
	settings := DefaultDohSettings()
	pinned := &tls.Config{ServerName: "pinned.example"}
	settings.DnsResolverSettings.TlsConfig = pinned

	httpClient := httpClientWithDialer(settings, settings.DialContext, tls.NewLRUClientSessionCache(dohTlsSessionCacheCapacity))
	tr := httpClient.Transport.(*http.Transport)
	if tr.TLSClientConfig.ClientSessionCache == nil {
		t.Fatalf("doh client tls config must carry a session cache")
	}
	if tr.TLSClientConfig.ServerName != "pinned.example" {
		t.Fatalf("cloned tls config must keep the pinned server name")
	}
	if pinned.ClientSessionCache != nil {
		t.Fatalf("caller tls config must not be mutated")
	}
	if tr.IdleConnTimeout != 15*time.Minute {
		t.Fatalf("idle conn timeout must be 15m, got %v", tr.IdleConnTimeout)
	}

	// a config that already has a cache keeps it
	ownCache := tls.NewLRUClientSessionCache(4)
	settings.DnsResolverSettings.TlsConfig = &tls.Config{ClientSessionCache: ownCache}
	httpClient = httpClientWithDialer(settings, settings.DialContext, tls.NewLRUClientSessionCache(dohTlsSessionCacheCapacity))
	tr = httpClient.Transport.(*http.Transport)
	if tr.TLSClientConfig.ClientSessionCache != tls.ClientSessionCache(ownCache) {
		t.Fatalf("existing session cache must be kept")
	}
}

// TestServerStatsSeed pins seed/scores: a seeded server is preferred in the
// fan-out order, the seed decays on the normal windows, and scores round-trip
// (clamped) for persistence.
func TestServerStatsSeed(t *testing.T) {
	now := time.Now()
	urls := []string{"https://a/dns-query", "https://b/dns-query"}

	stats := newServerStats()
	stats.seed(map[string]float64{urls[0]: 100.0})

	// clamped on seed
	scores := stats.scores()
	if scores[urls[0]] < dohSeedMaxScore-0.5 || dohSeedMaxScore+0.5 < scores[urls[0]] {
		t.Fatalf("seed must clamp to dohSeedMaxScore, got %v", scores[urls[0]])
	}
	if _, ok := scores[urls[1]]; ok {
		t.Fatalf("unseeded server must have no score")
	}

	// the seeded server wins the weighted order nearly always (weight ~8 vs floor 0.05)
	firstCount := 0
	trials := 200
	for range trials {
		if stats.orderAt(urls, now)[0] == urls[0] {
			firstCount += 1
		}
	}
	if firstCount < trials*9/10 {
		t.Fatalf("seeded server should lead the order, led %d/%d", firstCount, trials)
	}

	// nil-safe
	var nilStats *serverStats
	nilStats.seed(map[string]float64{urls[0]: 1})
	if nilStats.scores() != nil {
		t.Fatalf("nil stats scores must be nil")
	}
}

// TestDohCacheWarm pins Warm: it opens a connection by answering a real wire
// query, reports success, records the server into the scores (so a warm alone
// seeds the next session), and never pollutes the answer cache.
func TestDohCacheWarm(t *testing.T) {
	server, dohUrl, tlsConfig := newWarmTestDohServer(t)
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 15 * time.Second
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{dohUrl},
		TlsConfig:         tlsConfig,
	}
	cache := NewDohCache(settings)
	defer cache.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !cache.Warm(ctx, 2) {
		t.Fatalf("warm against a live server must succeed")
	}
	scores := cache.ServerScores()
	if scores[dohUrl] <= 0 {
		t.Fatalf("warm must record the server score, got %v", scores)
	}
	func() {
		cache.stateLock.Lock()
		defer cache.stateLock.Unlock()
		if 0 < len(cache.queryResultExpiration) {
			t.Fatalf("warm must not populate the answer cache")
		}
	}()

	// a local-doh cache (the mux fallback shape) warms over the local client
	localSettings := DefaultDohSettings()
	localSettings.RequestTimeout = 15 * time.Second
	localSettings.DnsResolverSettings = &DnsResolverSettings{
		EnableLocalDoh:   true,
		LocalDohUrlsIpv4: []string{dohUrl},
		TlsConfig:        tlsConfig,
	}
	localCache := NewDohCache(localSettings)
	defer localCache.Close()
	if !localCache.Warm(ctx, 2) {
		t.Fatalf("local-doh warm must succeed")
	}

	// warm against nothing fails cleanly
	deadSettings := DefaultDohSettings()
	deadSettings.RequestTimeout = 1 * time.Second
	deadSettings.DnsResolverSettings = &DnsResolverSettings{}
	deadCache := NewDohCache(deadSettings)
	defer deadCache.Close()
	if deadCache.Warm(ctx, 2) {
		t.Fatalf("warm with no servers must fail")
	}
}

// TestDohCacheSeedPersistRoundTrip pins the cross-session flow: scores from a
// used cache seed a fresh cache, which prefers the seeded server immediately.
func TestDohCacheSeedPersistRoundTrip(t *testing.T) {
	server, dohUrl, tlsConfig := newWarmTestDohServer(t)
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 15 * time.Second
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{dohUrl},
		TlsConfig:         tlsConfig,
	}
	cache := NewDohCache(settings)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrs, authoritative := cache.QueryResult(ctx, "A", "seed.roundtrip.test")
	if len(addrs) == 0 || !authoritative {
		t.Fatalf("query must resolve")
	}
	scores := cache.ServerScores()
	cache.Close()
	if scores[dohUrl] <= 0 {
		t.Fatalf("used cache must have scores")
	}

	seededSettings := DefaultDohSettings()
	seededSettings.ServerStatsSeed = scores
	seededSettings.DnsResolverSettings = settings.DnsResolverSettings
	seeded := NewDohCache(seededSettings)
	defer seeded.Close()
	seededScores := seeded.ServerScores()
	if seededScores[dohUrl] <= 0 {
		t.Fatalf("seeded cache must start with the persisted score, got %v", seededScores)
	}
}
