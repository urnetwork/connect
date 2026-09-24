// Empty answers, cancellation and unrelated owners retain their meaning when
// a datagram dial consumes only the first usable DNS family.
package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
)

// An empty or failed first family is not usable and cannot cancel the later
// family. Publication of that later answer, not elapsed time, releases the dial.
func checkDohFirstAnswerUdpNoAnswer(t *testing.T, authoritative bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		cache, _ := newDohFirstAnswerCache(false)
		defer cache.Close()
		delete(cache.queryResultExpiration, NewDohKey("A", "first-answer.example"))
		first := &dohFlight{done: make(chan struct{}), authoritative: authoritative}
		close(first.done)
		cache.inflight[NewDohKey("A", "first-answer.example")] = first
		held := cache.inflight[NewDohKey("AAAA", "first-answer.example")]
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		resolver := &internalDohResolver{cache: cache}
		result := make(chan *net.UDPAddr, 1)
		errors := make(chan error, 1)
		go func() {
			addr, err := resolver.resolveUDPAddr(ctx, "first-answer.example:443")
			result <- addr
			errors <- err
		}()
		synctest.Wait()
		select {
		case <-result:
			t.Fatal("empty or failed first family prematurely finished the UDP dial")
		default:
		}
		want := netip.MustParseAddr("2001:db8::92")
		held.addrs = []netip.Addr{want}
		held.authoritative = true
		close(held.done)
		addr, err := <-result, <-errors
		if err != nil || addr == nil || !addr.IP.Equal(net.IP(want.AsSlice())) {
			t.Fatalf("later usable family result=%v err=%v", addr, err)
		}
	})
}

// Authoritative NODATA for A cannot erase the still-pending AAAA answer.
func TestDohFirstAnswerUdpEmptyFirstKeepsPendingFamily(t *testing.T) {
	checkDohFirstAnswerUdpNoAnswer(t, true)
}

// A transport failure for A must also leave the other family available.
func TestDohFirstAnswerUdpFailedFirstKeepsPendingFamily(t *testing.T) {
	checkDohFirstAnswerUdpNoAnswer(t, false)
}

// Without any usable address, only two authoritative answers establish absence.
func TestDohFirstAnswerUdpNoUsableResultPreservesErrorKind(t *testing.T) {
	for _, authoritative := range [][2]bool{{true, true}, {true, false}, {false, true}, {false, false}} {
		func() {
			cache, _ := newDohFirstAnswerCache(false)
			defer cache.Close()
			delete(cache.queryResultExpiration, NewDohKey("A", "first-answer.example"))
			for index, recordType := range []string{"A", "AAAA"} {
				flight := &dohFlight{done: make(chan struct{}), authoritative: authoritative[index]}
				close(flight.done)
				cache.inflight[NewDohKey(recordType, "first-answer.example")] = flight
			}
			resolver := &internalDohResolver{cache: cache}
			addr, err := resolver.resolveUDPAddr(t.Context(), "first-answer.example:443")
			var dnsErr *net.DNSError
			wantNotFound := authoritative[0] && authoritative[1]
			if addr != nil || !errors.As(err, &dnsErr) || dnsErr.IsNotFound != wantNotFound || dnsErr.IsTemporary == wantNotFound {
				t.Fatalf("authority=%v result=%v err=%v, want not_found=%t", authoritative, addr, err, wantNotFound)
			}
		}()
	}
}

// Cancellation releases both of this call's waiting query workers without
// inventing an address or taking ownership of the foreign resolver flight.
func TestDohFirstAnswerUdpCancellationKeepsForeignOwners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, _ := newDohFirstAnswerCache(false)
		defer cache.Close()
		delete(cache.queryResultExpiration, NewDohKey("A", "first-answer.example"))
		first := &dohFlight{done: make(chan struct{})}
		cache.inflight[NewDohKey("A", "first-answer.example")] = first
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		resolver := &internalDohResolver{cache: cache}
		done := make(chan error, 1)
		go func() {
			addr, err := resolver.resolveUDPAddr(ctx, "first-answer.example:443")
			if addr != nil {
				done <- errors.New("cancellation fabricated a UDP address")
				return
			}
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("UDP cancellation error=%v", err)
		}
		for _, recordType := range []string{"A", "AAAA"} {
			flight := cache.inflight[NewDohKey(recordType, "first-answer.example")]
			select {
			case <-flight.done:
				t.Fatalf("UDP caller canceled the foreign %s owner", recordType)
			default:
			}
		}
	})
}

// Returning a usable A must not retire a different caller already waiting
// for AAAA on the same explicitly shared cache.
func TestDohFirstAnswerUdpWinnerPreservesIndependentWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, readyAddr := newDohFirstAnswerCache(false)
		defer cache.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		other := make(chan []netip.Addr, 1)
		go func() {
			addrs, _ := cache.QueryResult(ctx, "AAAA", "first-answer.example")
			other <- addrs
		}()
		synctest.Wait()
		resolver := &internalDohResolver{cache: cache}
		done := make(chan *net.UDPAddr, 1)
		go func() {
			addr, _ := resolver.resolveUDPAddr(ctx, "first-answer.example:443")
			done <- addr
		}()
		synctest.Wait()
		select {
		case addr := <-done:
			if addr == nil || !addr.IP.Equal(net.IP(readyAddr.AsSlice())) {
				t.Fatalf("ready UDP address=%v", addr)
			}
		default:
			t.Error("first UDP answer waited for an independent caller's family")
			cancel()
			<-done
			<-other
			return
		}
		select {
		case <-other:
			t.Fatal("UDP winner terminated the independent AAAA waiter")
		default:
		}
		held := cache.inflight[NewDohKey("AAAA", "first-answer.example")]
		held.addrs = []netip.Addr{netip.MustParseAddr("2001:db8::93")}
		held.authoritative = true
		close(held.done)
		if addrs := <-other; len(addrs) != 1 || addrs[0] != held.addrs[0] {
			t.Fatalf("independent waiter lost its later answer: %v", addrs)
		}
	})
}
