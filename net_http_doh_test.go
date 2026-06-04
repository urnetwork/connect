package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"

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

	dohCache := NewDohCache(settings)

	for range 3 {
		ips := dohCache.Query(ctx, "A", "missing.example")
		assert.Equal(t, len(ips), 0)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&requestCount))
}
