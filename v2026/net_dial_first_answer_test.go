// First-answer controls force one usable family to coexist with an unrelated
// pending flight. Virtual quiescence proves progress before any fallback timer.
package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// Owns one ready cache entry and one unfinished foreign query. There is no
// network or global resolver change; cancellation must release only our waiter.
func newDohFirstAnswerCache(readyIpv6 bool) (*DohCache, netip.Addr) {
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	settings.RequestTimeout = time.Hour
	settings.DnsResolverSettings = &DnsResolverSettings{}
	cache := NewDohCache(settings)
	readyType, heldType := "A", "AAAA"
	readyAddr := netip.MustParseAddr("192.0.2.91")
	if readyIpv6 {
		readyType, heldType = heldType, readyType
		readyAddr = netip.MustParseAddr("2001:db8::91")
	}
	cache.queryResultExpiration[NewDohKey(readyType, "first-answer.example")] = &DohResult{
		Time: time.Now(),
		AddrExpirations: map[netip.Addr]time.Time{
			readyAddr: time.Now().Add(time.Hour),
		},
	}
	cache.inflight[NewDohKey(heldType, "first-answer.example")] = &dohFlight{done: make(chan struct{})}
	return cache, readyAddr
}

// The first completed family starts immediately; a stagger owns only later
// connection attempts. The pending family is still available if TCP fails.
func checkDohFirstAnswerTcp(t *testing.T, readyIpv6 bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		cache, readyAddr := newDohFirstAnswerCache(readyIpv6)
		defer cache.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := time.Now()
		dialed := make(chan netip.Addr, 1)
		done := make(chan error, 1)
		go func() {
			conn, err := dialDohAddrsRace(ctx, cache, "tcp", "first-answer.example", time.Hour, func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
				dialed <- addr
				client, peer := net.Pipe()
				_ = peer.Close()
				return client, nil
			})
			if conn != nil {
				_ = conn.Close()
			}
			done <- err
		}()
		synctest.Wait()
		select {
		case addr := <-dialed:
			if addr != readyAddr || !time.Now().Equal(started) {
				t.Errorf("first TCP address=%s elapsed=%s, want ready=%s without a family wait", addr, time.Since(started), readyAddr)
			}
		default:
			t.Error("usable first TCP family waited for the other DNS family or fallback timer")
		}
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

// Ready A must not pay the old pending-AAAA preference delay.
func TestDohFirstAnswerTcpReadyIpv4(t *testing.T) {
	checkDohFirstAnswerTcp(t, false)
}

// Ready AAAA is the symmetric healthy control.
func TestDohFirstAnswerTcpReadyIpv6(t *testing.T) {
	checkDohFirstAnswerTcp(t, true)
}

// Exercises the real protected-name UDP entrypoints, not an unused resolver
// helper. A first address is sufficient for a datagram socket, never path proof.
func checkDohFirstAnswerUdp(t *testing.T, readyIpv6 bool, wrapped bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		cache, readyAddr := newDohFirstAnswerCache(readyIpv6)
		defer cache.Close()
		resolver := &internalDohResolver{cache: cache, domains: []string{"first-answer.example"}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := time.Now()
		type outcome struct {
			addr netip.Addr
			err  error
		}
		done := make(chan outcome, 1)
		go func() {
			if wrapped {
				var addr netip.Addr
				dial := resolver.wrapDialContext(func(ctx context.Context, network string, address string) (net.Conn, error) {
					host, _, err := net.SplitHostPort(address)
					if err != nil {
						return nil, err
					}
					addr, err = netip.ParseAddr(host)
					if err != nil || network != familyDialNetwork("udp", readyAddr) {
						return nil, errors.New("UDP wrapper changed the ready family")
					}
					client, peer := net.Pipe()
					_ = peer.Close()
					return client, nil
				})
				conn, err := dial(ctx, "udp", "first-answer.example:443")
				if conn != nil {
					_ = conn.Close()
				}
				done <- outcome{addr: addr, err: err}
				return
			}
			addr, err := resolver.resolveUDPAddr(ctx, "first-answer.example:443")
			if err != nil {
				done <- outcome{err: err}
				return
			}
			resolved, _ := netip.AddrFromSlice(addr.IP)
			done <- outcome{addr: resolved.Unmap()}
		}()
		synctest.Wait()
		select {
		case result := <-done:
			if result.err != nil || result.addr != readyAddr || !time.Now().Equal(started) {
				t.Errorf("first UDP result=%s err=%v elapsed=%s, want ready=%s without waiting", result.addr, result.err, time.Since(started), readyAddr)
			}
		default:
			t.Error("usable first UDP family waited for the other DNS family")
			cancel()
			<-done
		}
	})
}

// A single-address control UDP caller must use the ready A result immediately.
func TestDohFirstAnswerUdpReadyIpv4(t *testing.T) {
	checkDohFirstAnswerUdp(t, false, false)
}

// A single-address control UDP caller must also use ready AAAA immediately.
func TestDohFirstAnswerUdpReadyIpv6(t *testing.T) {
	checkDohFirstAnswerUdp(t, true, false)
}

// Generic wrapped UDP must not accidentally keep the bulk resolve barrier.
func TestDohFirstAnswerWrappedUdpReadyIpv4(t *testing.T) {
	checkDohFirstAnswerUdp(t, false, true)
}

// The wrapped path has no implicit IPv4 preference when AAAA is ready first.
func TestDohFirstAnswerWrappedUdpReadyIpv6(t *testing.T) {
	checkDohFirstAnswerUdp(t, true, true)
}
