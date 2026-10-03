// Real resolver and tunnel boundaries report only the requested connection's
// progress; barriers establish phases without timing-based negative assertions.
package connect

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The lifetime join must retain only the explicit request-local observation.
func TestTunDialObservationSurvivesLifetimeJoin(t *testing.T) {
	var observed atomic.Int32
	ctx := WithTunDialObserver(t.Context(), func(TunDialEvent) { observed.Add(1) })
	tun := &Tun{ctx: t.Context()}
	joined, cancel := tun.dialCtx(ctx)
	defer cancel()
	if observer := tunDialObserver(joined); observer == nil {
		t.Fatal("tunnel lifetime join lost its request observer")
	} else {
		observer(TunDialDnsStarted)
	}
	if observed.Load() != 1 {
		t.Fatal("request observer was not retained exactly once")
	}
}

// A lifetime context is not a request owner, even if it carries an observer.
// A nested resolver clearing its caller observer must stay unobserved.
func TestTunDialObservationDoesNotBorrowLifetimeObserver(t *testing.T) {
	lifetime := WithTunDialObserver(t.Context(), func(TunDialEvent) {
		t.Error("a request borrowed its tunnel lifetime's observer")
	})
	tun := &Tun{ctx: lifetime}
	joined, cancel := tun.dialCtx(t.Context())
	defer cancel()
	if tunDialObserver(joined) != nil {
		t.Fatal("tunnel lifetime supplied an observer absent from the request")
	}
}

// A live DNS request with no answer cannot claim a target TCP connection,
// even though the private DoH request itself necessarily used a TCP socket.
func TestTunDialObservationHeldDnsDoesNotReportTargetTcp(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{})
	cache := newDohProgressCache(t, func(_ http.ResponseWriter, request *http.Request, _ dnsmessage.Type) {
		close(entered)
		<-request.Context().Done()
	})
	var events [4]atomic.Int32
	ctx = WithTunDialObserver(ctx, func(event TunDialEvent) { events[event-1].Add(1) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = dialDohAddrsRace(ctx, cache, "tcp4", "held-observation.example", time.Hour,
			func(context.Context, netip.Addr) (net.Conn, error) {
				t.Error("target dial started before its DNS answer")
				return nil, context.Canceled
			})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("resolver did not reach its barrier")
	}
	if events[0].Load() != 1 || events[1].Load() != 0 || events[2].Load() != 0 || events[3].Load() != 0 {
		t.Errorf("held DNS progress = %d/%d/%d/%d, want1/0/0/0", events[0].Load(), events[1].Load(), events[2].Load(), events[3].Load())
	}
	cancel()
	<-done
}

// A usable answer advances to the target TCP phase before that socket returns.
func TestTunDialObservationAnswerThenHeldTcp(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cache := newDohProgressCache(t, func(writer http.ResponseWriter, request *http.Request, _ dnsmessage.Type) {
		writeDohWire(writer, request, []netip.Addr{netip.MustParseAddr("192.0.2.91")}, 60, false)
	})
	var events [4]atomic.Int32
	ctx = WithTunDialObserver(ctx, func(event TunDialEvent) { events[event-1].Add(1) })
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = dialDohAddrsRace(ctx, cache, "tcp4", "tcp-observation.example", time.Hour,
			func(ctx context.Context, _ netip.Addr) (net.Conn, error) {
				if tunDialObserver(ctx) != nil {
					t.Error("target observer escaped into a nested transport context")
				}
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("target TCP did not reach its barrier")
	}
	if events[0].Load() != 1 || events[1].Load() != 1 || events[2].Load() != 1 || events[3].Load() != 0 {
		t.Errorf("held TCP progress = %d/%d/%d/%d, want1/1/1/0", events[0].Load(), events[1].Load(), events[2].Load(), events[3].Load())
	}
	cancel()
	<-done
}

// The real tunnel starts its usable family without waiting for the other DNS
// answer, and the resolver's own TCP connection is not counted as the target.
func TestTunDialObservationKeepsFirstAnswerProgress(t *testing.T) {
	fixture := newDohProgressTunFixture(t, 4)
	var events [4]atomic.Int32
	fixture.ctx = WithTunDialObserver(fixture.ctx, func(event TunDialEvent) { events[event-1].Add(1) })
	fixture.dial = fixture.left.DialContext
	fixture.dialHeldFamily(t, false)
	if events[0].Load() != 1 || events[1].Load() != 1 || events[2].Load() != 1 || events[3].Load() != 1 {
		t.Errorf("target progress includes resolver/loser work or lost caller scope: %d/%d/%d/%d", events[0].Load(), events[1].Load(), events[2].Load(), events[3].Load())
	}
}
