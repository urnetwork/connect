// Resolver progress, fallback, and ownership controls complement the real tunnel barrier tests.
package connect

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Creates the real RFC 8484 parser/cache over one caller-selected test endpoint.
func newDohProgressCache(t *testing.T, answer func(http.ResponseWriter, *http.Request, dnsmessage.Type)) *DohCache {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		wire, err := base64.RawURLEncoding.DecodeString(request.URL.Query().Get("dns"))
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var parser dnsmessage.Parser
		if _, err := parser.Start(wire); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		question, err := parser.Question()
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		answer(writer, request, question.Type)
	}))
	t.Cleanup(server.Close)
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	settings.RequestTimeout = 5 * time.Second
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{server.URL},
	}
	cache := NewDohCache(settings)
	t.Cleanup(cache.Close)
	return cache
}

// A definitive TCP failure must keep the other DNS family available, even
// when that answer can only finish after the first family's TCP attempt.
func TestDohDialProgressRetainsPendingFamilyAfterTcpFailure(t *testing.T) {
	for _, firstIpv6 := range []bool{true, false} {
		func() {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			firstAddr := netip.MustParseAddr("192.0.2.81")
			secondAddr := netip.MustParseAddr("2001:db8::81")
			firstType := dnsmessage.TypeA
			if firstIpv6 {
				firstAddr, secondAddr = secondAddr, firstAddr
				firstType = dnsmessage.TypeAAAA
			}
			heldEntered := make(chan struct{})
			firstTcp := make(chan struct{})
			var readyQueries, heldQueries, firstDials, secondDials atomic.Int64
			cache := newDohProgressCache(t, func(writer http.ResponseWriter, request *http.Request, recordType dnsmessage.Type) {
				if recordType == firstType {
					readyQueries.Add(1)
					select {
					case <-heldEntered:
					case <-request.Context().Done():
						return
					}
					writeDohWire(writer, request, []netip.Addr{firstAddr}, 60, false)
					return
				}
				heldQueries.Add(1)
				close(heldEntered)
				select {
				case <-firstTcp:
				case <-request.Context().Done():
					return
				}
				writeDohWire(writer, request, []netip.Addr{secondAddr}, 60, false)
			})
			refused := errors.New("synthetic first-family refusal")
			conn, err := dialDohAddrsRace(ctx, cache, "tcp", "fallback.example", DefaultDialFallbackDelay, func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
				if addr == firstAddr {
					firstDials.Add(1)
					close(firstTcp)
					return nil, refused
				}
				if addr != secondAddr {
					return nil, errors.New("unexpected resolved address")
				}
				secondDials.Add(1)
				client, peer := net.Pipe()
				_ = peer.Close()
				return client, nil
			})
			if err != nil {
				t.Fatalf("first_ipv6=%t: %v", firstIpv6, err)
			}
			_ = conn.Close()
			if firstDials.Load() != 1 || secondDials.Load() != 1 || readyQueries.Load() != 1 || heldQueries.Load() != 1 {
				t.Fatalf("first_ipv6=%t: first_dials=%d second_dials=%d ready_queries=%d held_queries=%d", firstIpv6, firstDials.Load(), secondDials.Load(), readyQueries.Load(), heldQueries.Load())
			}
		}()
	}
}

// Buffered answers retain publication order for the first attempt. An
// already-published A must not wait for, or be displaced by, later AAAA.
func TestDohDialProgressReadyPairUsesFirstPublishedFamily(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ipv4 := netip.MustParseAddr("192.0.2.82")
	ipv6 := netip.MustParseAddr("2001:db8::82")
	results := make(chan dohDialQueryResult, 2)
	results <- dohDialQueryResult{addrs: []netip.Addr{ipv4}, authoritative: true}
	results <- dohDialQueryResult{addrs: []netip.Addr{ipv6}, authoritative: true}
	var dials atomic.Int64
	conn, err := dialAddrsRaceWithResolution(ctx, nil, time.Hour, func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
		dials.Add(1)
		if addr != ipv4 {
			return nil, errors.New("later IPv6 displaced the first published IPv4 answer")
		}
		client, peer := net.Pipe()
		_ = peer.Close()
		return client, nil
	}, &dialAddrResolution{results: results, pending: 2, host: "ready.example"})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if dials.Load() != 1 {
		t.Fatalf("dials=%d, want exactly the preferred family", dials.Load())
	}
}

// Concurrent cache hits use their first completed family without re-querying.
func TestDohDialProgressCachedPairKeepsResolver(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ipv4 := netip.MustParseAddr("192.0.2.83")
	ipv6 := netip.MustParseAddr("2001:db8::83")
	var queries, dials atomic.Int64
	cache := newDohProgressCache(t, func(writer http.ResponseWriter, request *http.Request, recordType dnsmessage.Type) {
		queries.Add(1)
		writeDohWire(writer, request, []netip.Addr{ipv4, ipv6}, 60, false)
	})
	for _, recordType := range []string{"A", "AAAA"} {
		addrs, authoritative := cache.QueryResult(ctx, recordType, "cached.example")
		if !authoritative || len(addrs) != 1 {
			t.Fatal("failed to populate resolver cache")
		}
	}
	conn, err := dialDohAddrsRace(ctx, cache, "tcp", "cached.example", time.Hour, func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
		dials.Add(1)
		if addr != ipv4 && addr != ipv6 {
			return nil, errors.New("cached dial did not use either configured answer")
		}
		client, peer := net.Pipe()
		_ = peer.Close()
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if queries.Load() != 2 || dials.Load() != 1 {
		t.Fatalf("queries=%d dials=%d", queries.Load(), dials.Load())
	}
}

// The progressive owner joins its canceled TCP worker and both resolver
// callers; cache retirement additionally joins detached HTTP transport owners.
func TestDohDialProgressCancellationJoinsPendingOwners(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watchdog, watchdogCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer watchdogCancel()
	heldEntered := make(chan struct{})
	heldCanceled := make(chan struct{})
	tcpEntered := make(chan struct{})
	tcpExited := make(chan struct{})
	var once sync.Once
	cache := newDohProgressCache(t, func(writer http.ResponseWriter, request *http.Request, recordType dnsmessage.Type) {
		if recordType == dnsmessage.TypeAAAA {
			select {
			case <-heldEntered:
			case <-request.Context().Done():
				return
			}
			writeDohWire(writer, request, []netip.Addr{netip.MustParseAddr("2001:db8::84")}, 60, false)
			return
		}
		close(heldEntered)
		<-request.Context().Done()
		close(heldCanceled)
	})
	result := make(chan error, 1)
	go func() {
		conn, err := dialDohAddrsRace(ctx, cache, "tcp", "cancel.example", time.Hour, func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
			once.Do(func() { close(tcpEntered) })
			<-ctx.Done()
			close(tcpExited)
			return nil, ctx.Err()
		})
		if conn != nil {
			_ = conn.Close()
		}
		result <- err
	}()
	select {
	case <-tcpEntered:
	case <-watchdog.Done():
		t.Fatal("ready TCP family never started")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want caller cancellation", err)
		}
	case <-watchdog.Done():
		t.Fatal("cancellation did not join")
	}
	select {
	case <-tcpExited:
	default:
		t.Fatal("TCP owner survived the dial result")
	}
	cache.stateLock.Lock()
	inflightCount := len(cache.inflight)
	cache.stateLock.Unlock()
	if inflightCount != 0 {
		t.Fatalf("resolver callers survived return: %d", inflightCount)
	}
	cache.Close()
	select {
	case <-heldCanceled:
	case <-watchdog.Done():
		t.Fatal("held HTTP request was not canceled")
	}
}

// A winning later family cancels and closes a connection returned by the
// losing TCP worker; the peer must observe EOF before the race returns.
func TestDohDialProgressClosesLateSuccessfulLoserBeforeReturn(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ipv4 := netip.MustParseAddr("192.0.2.85")
	ipv6 := netip.MustParseAddr("2001:db8::85")
	cache := newDohProgressCache(t, func(writer http.ResponseWriter, request *http.Request, recordType dnsmessage.Type) {
		writeDohWire(writer, request, []netip.Addr{ipv4, ipv6}, 60, false)
	})
	for _, recordType := range []string{"A", "AAAA"} {
		if _, authoritative := cache.QueryResult(ctx, recordType, "loser.example"); !authoritative {
			t.Fatal("cache setup failed")
		}
	}
	firstEntered := make(chan struct{})
	loserPeer := make(chan net.Conn, 1)
	var dials atomic.Int64
	conn, err := dialDohAddrsRace(ctx, cache, "tcp", "loser.example", DefaultDialFallbackDelay, func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
		client, peer := net.Pipe()
		if dials.Add(1) == 1 {
			loserPeer <- peer
			close(firstEntered)
			<-ctx.Done()
			return client, nil
		}
		select {
		case <-firstEntered:
		case <-ctx.Done():
			_ = client.Close()
			_ = peer.Close()
			return nil, ctx.Err()
		}
		_ = peer.Close()
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	peer := <-loserPeer
	defer peer.Close()
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("losing peer read=%v, want EOF", err)
	}
}

// Only two authoritative empty answers establish absence. An unavailable
// family remains a temporary resolution error, and no empty result starts TCP.
func TestDohDialProgressEmptyAnswersPreserveErrorMeaning(t *testing.T) {
	for _, test := range []struct {
		name              string
		ipv4Authoritative bool
		ipv6Authoritative bool
	}{
		{name: "both empty", ipv4Authoritative: true, ipv6Authoritative: true},
		{name: "empty A and failed AAAA", ipv4Authoritative: true},
		{name: "failed A and empty AAAA", ipv6Authoritative: true},
		{name: "both failed"},
	} {
		func() {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var queries, dials atomic.Int64
			cache := newDohProgressCache(t, func(writer http.ResponseWriter, request *http.Request, recordType dnsmessage.Type) {
				queries.Add(1)
				authoritative := test.ipv4Authoritative
				if recordType == dnsmessage.TypeAAAA {
					authoritative = test.ipv6Authoritative
				}
				if !authoritative {
					writer.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				writeDohWire(writer, request, nil, 60, false)
			})
			conn, err := dialDohAddrsRace(ctx, cache, "tcp", "empty.example", DefaultDialFallbackDelay, func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
				dials.Add(1)
				return nil, errors.New("unexpected TCP attempt with no answer")
			})
			if conn != nil {
				_ = conn.Close()
				t.Fatalf("%s: connection without an address", test.name)
			}
			var dnsErr *net.DNSError
			wantNotFound := test.ipv4Authoritative && test.ipv6Authoritative
			if !errors.As(err, &dnsErr) || dnsErr.IsNotFound != wantNotFound || dnsErr.IsTemporary == wantNotFound {
				t.Fatalf("%s: error=%v, want not_found=%t temporary=%t", test.name, err, wantNotFound, !wantNotFound)
			}
			if queries.Load() != 2 || dials.Load() != 0 {
				t.Fatalf("%s: queries=%d dials=%d, want two queries and no TCP", test.name, queries.Load(), dials.Load())
			}
		}()
	}
}

// Holding the warmed cache's entry lock creates an explicit entry barrier:
// an already-canceled caller must return without consulting either DNS entry.
func TestDohDialProgressAlreadyCanceledDoesNotStartCachedLookup(t *testing.T) {
	watchdog, watchdogCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer watchdogCancel()
	var queries, dials atomic.Int64
	cache := newDohProgressCache(t, func(writer http.ResponseWriter, request *http.Request, recordType dnsmessage.Type) {
		queries.Add(1)
		writeDohWire(writer, request, []netip.Addr{netip.MustParseAddr("192.0.2.86"), netip.MustParseAddr("2001:db8::86")}, 60, false)
	})
	for _, recordType := range []string{"A", "AAAA"} {
		addrs, authoritative := cache.QueryResult(watchdog, recordType, "canceled.example")
		if !authoritative || len(addrs) != 1 {
			t.Fatal("failed to warm both cache entries")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := make(chan error, 1)
	cache.stateLock.Lock()
	go func() {
		conn, err := dialDohAddrsRace(ctx, cache, "tcp", "canceled.example", DefaultDialFallbackDelay, func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("already-canceled caller started TCP")
		})
		if conn != nil {
			_ = conn.Close()
		}
		result <- err
	}()
	var err error
	select {
	case err = <-result:
		cache.stateLock.Unlock()
	case <-watchdog.Done():
		cache.stateLock.Unlock()
		<-result
		t.Fatal("already-canceled caller consulted the locked DNS cache")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want unchanged cancellation identity", err)
	}
	if queries.Load() != 2 || dials.Load() != 0 {
		t.Fatalf("queries=%d dials=%d after cancellation", queries.Load(), dials.Load())
	}
}

// A narrowed stream network starts only its permitted record query and TCP family.
func TestDohDialProgressHonorsExplicitFamily(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		func() {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var ipv4Queries, ipv6Queries, dials atomic.Int64
			cache := newDohProgressCache(t, func(writer http.ResponseWriter, request *http.Request, recordType dnsmessage.Type) {
				if recordType == dnsmessage.TypeA {
					ipv4Queries.Add(1)
				} else {
					ipv6Queries.Add(1)
				}
				writeDohWire(writer, request, []netip.Addr{netip.MustParseAddr("192.0.2.87"), netip.MustParseAddr("2001:db8::87")}, 60, false)
			})
			conn, err := dialDohAddrsRace(ctx, cache, network, "family.example", time.Hour, func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
				dials.Add(1)
				if (network == "tcp4") != addr.Is4() {
					return nil, errors.New("dial crossed caller's explicit family")
				}
				client, peer := net.Pipe()
				_ = peer.Close()
				return client, nil
			})
			if err != nil {
				t.Fatalf("%s: %v", network, err)
			}
			_ = conn.Close()
			wantIpv4Queries, wantIpv6Queries := int64(1), int64(0)
			if network == "tcp6" {
				wantIpv4Queries, wantIpv6Queries = 0, 1
			}
			if dials.Load() != 1 || ipv4Queries.Load() != wantIpv4Queries || ipv6Queries.Load() != wantIpv6Queries {
				t.Fatalf("%s: ipv4_queries=%d ipv6_queries=%d dials=%d", network, ipv4Queries.Load(), ipv6Queries.Load(), dials.Load())
			}
		}()
	}
}
