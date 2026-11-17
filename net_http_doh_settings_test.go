// The DoH limits that were package constants are DohSettings fields (QD-702):
// their defaults are the values the constants had, and each cache, client and
// stats table reads its own instance's.
package connect

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDohSettingsDefaultsKeepTheFormerConstants(t *testing.T) {
	settings := DefaultDohSettings()
	AssertEqual(t, settings.ResponseMaxByteCount, ByteCount(16*1024))
	AssertEqual(t, settings.DnsLookupReserveByteCount, ByteCount(4*1024))
	AssertEqual(t, settings.StaleServeBound, 5*time.Minute)
	AssertEqual(t, settings.TlsSessionCacheCapacity, 16)
	if !slices.Equal(settings.ServerWindows, []time.Duration{5 * time.Minute, 15 * time.Minute, 60 * time.Minute}) {
		t.Fatalf("server windows %v", settings.ServerWindows)
	}
	AssertEqual(t, settings.ServerWeightFloor, 0.05)
	AssertEqual(t, settings.ServerSeedMaxScore, 8.0)

	// each call is a fresh value, so one owner's change reaches no other
	settings.ServerWindows[0] = time.Second
	AssertEqual(t, DefaultDohSettings().ServerWindows[0], 5*time.Minute)
}

func TestDohResponseMaxByteCountCutsTheRead(t *testing.T) {
	body := bytes.Repeat([]byte{0x42}, 100)
	transport := dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			Status:     "200 OK",
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    request,
		}, nil
	})
	for _, c := range []struct {
		responseMaxByteCount ByteCount
		want                 int
	}{
		{responseMaxByteCount: 10, want: 10},
		{responseMaxByteCount: 1000, want: 100},
		{responseMaxByteCount: 0, want: 0},
	} {
		client := &dohClient{httpClient: &http.Client{Transport: transport}, responseMaxByteCount: c.responseMaxByteCount}
		data, _, err := client.queryWireRawDetailedWithRoute(context.Background(), "https://doh.example/dns-query", dnsmessage.TypeA, "cut.example")
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != c.want {
			t.Errorf("a cap of %d read %d bytes, want %d", c.responseMaxByteCount, len(data), c.want)
		}
	}
}

func TestDohStaleServeBoundIsTheCallersOwn(t *testing.T) {
	now := time.Now()
	result := &DohResult{
		Time:            now.Add(-1 * time.Hour),
		AddrExpirations: map[netip.Addr]time.Time{netip.MustParseAddr("203.0.113.46"): now.Add(-10 * time.Minute)},
	}
	AssertEqual(t, result.staleUsable(now, DefaultDohSettings().StaleServeBound), false)
	AssertEqual(t, result.staleUsable(now, time.Hour), true)
	AssertEqual(t, result.staleUsable(now, 0), false)
}

func TestDohServerStatsTakeTheirSettings(t *testing.T) {
	const url = "https://seeded.example/dns-query"

	settings := DefaultDohSettings()
	settings.ServerSeedMaxScore = 2
	stats := newServerStats(settings)
	stats.seed(map[string]float64{url: 100})
	if score := stats.scores()[url]; score < 1.5 || 2.5 < score {
		t.Fatalf("a seed clamped to 2 scores %v", score)
	}

	// one window: a success counts once
	settings = DefaultDohSettings()
	settings.ServerWindows = []time.Duration{time.Hour}
	stats = newServerStats(settings)
	now := time.Unix(1_700_000_000, 0)
	stats.recordAt(url, true, now)
	score := func() float64 {
		stats.lock.Lock()
		defer stats.lock.Unlock()
		return stats.scoreLocked(url, now)
	}()
	AssertEqual(t, score, float64(1))

	// a non-positive seed max seeds nothing
	settings = DefaultDohSettings()
	settings.ServerSeedMaxScore = 0
	stats = newServerStats(settings)
	stats.seed(map[string]float64{url: 100})
	if scores := stats.scores(); len(scores) != 0 {
		t.Fatalf("a zero seed max seeded %v", scores)
	}
}

func TestDnsTargetHttpConcurrencyDividesByTheReservation(t *testing.T) {
	AssertEqual(t, dnsTargetHttpConcurrency(kib(160), kib(16), 4), 10)
	AssertEqual(t, dnsTargetHttpConcurrency(kib(160), kib(4), 4), 40)
	AssertEqual(t, dnsTargetHttpConcurrency(kib(16), kib(16), 4), 4)
	// no target, or no reservation to divide by, keeps the fallback
	AssertEqual(t, dnsTargetHttpConcurrency(0, kib(16), 4), 4)
	AssertEqual(t, dnsTargetHttpConcurrency(kib(160), 0, 4), 4)
}

// ForwardDnssec's OPT record advertises the cache's own cap, at most the
// 16-bit field's maximum.
func TestDohForwardDnssecAdvertisesTheCachesCap(t *testing.T) {
	question := testDnssecQuestion(t, "cap.dnssec.example.", dnsmessage.TypeA)
	for _, c := range []struct {
		responseMaxByteCount ByteCount
		wantPayload          uint16
	}{
		{responseMaxByteCount: 4096, wantPayload: 4096},
		{responseMaxByteCount: kib(128), wantPayload: 65535},
	} {
		server := newTestDnssecDohServer(t, testDnssecResponse(t, question, nil))
		settings := DefaultDohSettings()
		settings.RequestTimeout = 30 * time.Second
		settings.DohServerStagger = 0
		settings.ResponseMaxByteCount = c.responseMaxByteCount
		settings.DnsResolverSettings.EnableRemoteDoh = false
		settings.DnsResolverSettings.EnableLocalDoh = true
		settings.DnsResolverSettings.LocalDohUrlsIpv4 = []string{server.server.URL}
		settings.DnsResolverSettings.LocalDohUrlsIpv6 = []string{}
		cache := NewDohCache(settings)
		t.Cleanup(cache.Close)

		if _, usable := cache.ForwardDnssec(context.Background(), dnsmessage.TypeA, "cap.dnssec.example"); !usable {
			t.Fatalf("a cap of %d: ForwardDnssec refused a signed answer", c.responseMaxByteCount)
		}
		queries := server.queries()
		if len(queries) != 1 {
			t.Fatalf("the server took %d queries, want 1", len(queries))
		}
		var parser dnsmessage.Parser
		if _, err := parser.Start(queries[0]); err != nil {
			t.Fatal(err)
		}
		if err := parser.SkipAllQuestions(); err != nil {
			t.Fatal(err)
		}
		if err := parser.SkipAllAnswers(); err != nil {
			t.Fatal(err)
		}
		if err := parser.SkipAllAuthorities(); err != nil {
			t.Fatal(err)
		}
		opt, err := parser.AdditionalHeader()
		if err != nil {
			t.Fatal(err)
		}
		if uint16(opt.Class) != c.wantPayload {
			t.Errorf("a cap of %d advertised %d, want %d", c.responseMaxByteCount, opt.Class, c.wantPayload)
		}
	}
}
