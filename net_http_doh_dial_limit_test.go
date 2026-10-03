// Canceled resolver requests must not let reusable HTTP dials accumulate beyond
// the transport owner's budget. Connection reuse and sibling caches stay live.
package connect

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Socket establishment can succeed while TLS stalls. These handshakes must
// consume the same transport-owned cap even after their requests are canceled.
func TestDohDialLimitCoversDetachedTlsHandshakes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultDohSettings()
		settings.Log = NewNoopLogger()
		settings.MaxConcurrentHttpRequests = 2
		settings.RequestTimeout = time.Hour
		settings.TlsTimeout = time.Hour
		settings.DnsResolverSettings = &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: []string{"https://192.0.2.53/dns-query"},
		}
		peers := make(chan net.Conn, 20)
		var dials atomic.Int32
		settings.DialContextSettings = &DialContextSettings{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				client, peer := net.Pipe()
				dials.Add(1)
				peers <- peer
				return client, nil
			},
		}
		cache := NewDohCache(settings)
		defer cache.Close()
		defer func() {
			close(peers)
			for peer := range peers {
				_ = peer.Close()
			}
			synctest.Wait()
		}()
		for index := range 20 {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				cache.QueryResult(ctx, "A", fmt.Sprintf("handshake-%d.example", index))
			}()
			synctest.Wait()
			cancel()
			<-done
			synctest.Wait()
		}
		if count := dials.Load(); count != 2 {
			t.Errorf("canceled queries left %d TLS handshakes; want the transport cap of 2", count)
		}
	})
}

// The real HTTP transport detaches dials for reuse. Cancel every query after
// its workers are durably blocked, while leaving the cache and dials open.
func TestDohCanceledQueriesBoundDetachedDials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var started, finished atomic.Int32
		settings := DefaultDohSettings()
		settings.Log = NewNoopLogger()
		settings.MaxConcurrentHttpRequests = 2
		settings.RequestTimeout = time.Hour
		settings.DnsResolverSettings = &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: []string{"https://192.0.2.53/dns-query"},
		}
		settings.DialContextSettings = &DialContextSettings{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				started.Add(1)
				defer finished.Add(1)
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		cache := NewDohCache(settings)
		defer cache.Close()
		for index := range 20 {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				cache.QueryResult(ctx, "A", fmt.Sprintf("canceled-%d.example", index))
			}()
			synctest.Wait()
			cancel()
			<-done
			synctest.Wait()
		}
		if count := started.Load(); count != 2 {
			t.Errorf("canceled queries started %d detached dials; want the transport cap of 2", count)
		}
		if count := finished.Load(); count != 0 {
			t.Errorf("blocked reusable dials finished before cache retirement: %d", count)
		}
		cache.Close()
		if finished.Load() != started.Load() {
			t.Fatal("cache retirement did not join all admitted dials")
		}
	})
}

// A saturated cache cannot consume another cache's capacity or close its dials.
func TestDohDialLimitIsOwnedByEachCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var started, finished [2]atomic.Int32
		caches := [2]*DohCache{}
		cancels := [2]context.CancelFunc{}
		dones := [2]chan struct{}{}
		for index := range 2 {
			settings := DefaultDohSettings()
			settings.Log = NewNoopLogger()
			settings.MaxConcurrentHttpRequests = 1
			settings.RequestTimeout = time.Hour
			settings.DnsResolverSettings = &DnsResolverSettings{
				EnableRemoteDoh:   true,
				RemoteDohUrlsIpv4: []string{"https://192.0.2.53/dns-query"},
			}
			settings.DialContextSettings = &DialContextSettings{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					started[index].Add(1)
					defer finished[index].Add(1)
					<-ctx.Done()
					return nil, ctx.Err()
				},
			}
			caches[index] = NewDohCache(settings)
			defer caches[index].Close()
			ctx, cancel := context.WithCancel(context.Background())
			cancels[index] = cancel
			defer cancel()
			dones[index] = make(chan struct{})
			go func() {
				defer close(dones[index])
				caches[index].QueryResult(ctx, "A", "independent-owner.example")
			}()
		}
		synctest.Wait()
		if started[0].Load() != 1 || started[1].Load() != 1 {
			t.Fatal("independent caches shared dial admission")
		}
		cancels[0]()
		<-dones[0]
		caches[0].Close()
		synctest.Wait()
		if finished[0].Load() != 1 || finished[1].Load() != 0 {
			t.Fatal("closing one cache changed the sibling dial lifecycle")
		}
		cancels[1]()
		<-dones[1]
		caches[1].Close()
		if finished[1].Load() != 1 {
			t.Fatal("sibling cache did not retire its dial")
		}
	})
}

// A finite dial cap must preserve the established HTTP/2 connection instead of
// recreating one for each query or coupling its lifetime to a completed query.
func TestDohDialLimitPreservesHttp2Reuse(t *testing.T) {
	var dials atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("resolver request used HTTP/%d", r.ProtoMajor)
		}
		writeDohWire(w, r, []netip.Addr{netip.MustParseAddr("198.51.100.53")}, 60, false)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	settings.MaxConcurrentHttpRequests = 1
	settings.RequestTimeout = 5 * time.Second
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{server.URL + "/dns-query"},
		TlsConfig:         server.Client().Transport.(*http.Transport).TLSClientConfig,
	}
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	cache := NewDohCache(settings)
	defer cache.Close()
	for index := range 3 {
		addrs, authoritative := cache.QueryResult(t.Context(), "A", fmt.Sprintf("pooled-%d.example", index))
		if !authoritative || len(addrs) != 1 {
			t.Fatal("pooled resolver query did not answer")
		}
	}
	if count := dials.Load(); count != 1 {
		t.Fatalf("queries opened %d dials; want one reused HTTP/2 connection", count)
	}
}

// An unreachable resolver must not hold the healthy resolver's connection
// capacity. The latter also retains HTTP/1 keep-alive compatibility.
func TestDohDialLimitSaturatedOriginPreservesOtherOrigin(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "synthetic resolver answer")
	}))
	defer server.Close()
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	settings.MaxConcurrentHttpRequests = 1
	settings.RequestTimeout = time.Hour
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{"http://192.0.2.53/dns-query", server.URL + "/dns-query"},
	}
	started := make(chan struct{})
	var healthyDials atomic.Int32
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
			if address == "192.0.2.53:80" {
				close(started)
				<-dialCtx.Done()
				return nil, dialCtx.Err()
			}
			healthyDials.Add(1)
			return (&net.Dialer{}).DialContext(dialCtx, network, address)
		},
	}
	cache := NewDohCache(settings)
	defer cache.Close()
	requestCtx, requestCancel := context.WithCancel(ctx)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://192.0.2.53/dns-query", nil)
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan error, 1)
	go func() {
		response, err := cache.remoteClient.httpClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("blocked origin never began its dial")
	}
	requestCancel()
	if err := <-requestDone; err == nil {
		t.Fatal("canceled origin returned a response")
	}
	for range 3 {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/dns-query", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := cache.remoteClient.httpClient.Do(request)
		if err != nil {
			t.Fatalf("healthy origin blocked behind the other origin: %v", err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if count := healthyDials.Load(); count != 1 {
		t.Fatalf("healthy origin used %d dials; want one pooled connection", count)
	}
}
