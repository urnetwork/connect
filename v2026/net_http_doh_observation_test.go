// Synthetic barriers reproduce detached hedge completion and complete resolver
// results without public DNS, timing races, or retained production identities.
package connect

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// A failed literal dial must retain its family even under the opposite global
// policy. Only finite result tokens may survive an identity-bearing error.
func TestDohDialObservationIsFiniteAndLiteral(t *testing.T) {
	oldPolicy := ControlIpFamilyPolicy()
	SetControlIpFamilyPolicy(IpFamilyForce4)
	defer SetControlIpFamilyPolicy(oldPolicy)
	for _, test := range []struct {
		address string
		family  string
		err     error
		result  string
	}{
		{address: "192.0.2.53:443", family: "4", err: os.ErrDeadlineExceeded, result: "timeout"},
		{address: "192.0.2.56:443", family: "4", err: errors.New("i/o timeout"), result: "timeout"},
		{address: "[2001:db8::53]:443", family: "6", err: syscall.EAFNOSUPPORT, result: "unsupported_family"},
		{address: "[::ffff:192.0.2.53]:443", family: "4", err: syscall.ECONNREFUSED, result: "refused"},
		{address: "resolver.private.example:443", family: "unknown", err: context.Canceled, result: "canceled"},
		{address: "192.0.2.54", family: "4", err: nil, result: "success"},
		{address: "[2001:db8::54]:443", family: "6", err: errors.New("secret.fixture query.private.example 192.0.2.99 provider.fixture"), result: "error"},
	} {
		line := dohDialObservationLine(context.Background(), dohPathCaller, test.address, test.err)
		for _, want := range []string{"observable=v1 path=caller", "attempted_family=" + test.family, "result=" + test.result, "resolver_owner=unknown resolver_scope=unknown resolver_outcome=unknown"} {
			if !strings.Contains(line, want) {
				t.Errorf("line omitted %q: %s", want, line)
			}
		}
		if test.result == "timeout" && !strings.HasSuffix(line, " err=i/o timeout") {
			t.Fatal("older monitors would lose their exact timeout signature")
		}
		if test.result == "refused" && !strings.HasSuffix(line, " err=connect: connection refused") {
			t.Fatal("older monitors would lose their exact refusal signature")
		}
		for _, private := range []string{"192.0.2.", "2001:db8", "private.example", "secret.fixture", "provider.fixture"} {
			if strings.Contains(line, private) {
				t.Fatal("dial observation retained private input")
			}
		}
	}
}

// Custom source binding is not necessarily a tunnel; only the owning Tun can
// assert that path. Local fallback retains its own explicit host marker.
func TestDohDialObservationPathOwnership(t *testing.T) {
	settings := DefaultDohSettings()
	if path := dohRemoteDialPath(settings); path != dohPathHost {
		t.Fatalf("default path=%s", path)
	}
	settings.DialContextSettings = &DialContextSettings{}
	if path := dohRemoteDialPath(settings); path != dohPathCaller {
		t.Fatalf("custom path=%s", path)
	}
	resolver := &DnsResolverSettings{}
	tun := tunDohFamilyFixture(t, DefaultTunnelMtu, resolver, nil)
	if path := dohRemoteDialPath(tun.DohCache().settings); path != dohPathTun {
		t.Fatalf("owned tun path=%s", path)
	}
	ctx, observation := newDohResolverObservation(context.Background(), dohPathTun, dohScopeAddress)
	observation.outcome.Store(uint32(dohOutcomeAnswer))
	line := dohDialObservationLine(ctx, dohPathHost, "192.0.2.55:443", nil)
	if !strings.Contains(line, "path=host") || !strings.Contains(line, "resolver_owner=tun") {
		t.Fatalf("local fallback was confused with remote ownership: %s", line)
	}
}

// A real HTTP Transport detaches its dial from request cancellation. Hold the
// IPv6 dial until an IPv4 answer has returned through the complete cache, then
// release its timeout and join the cache lifecycle before reading evidence.
func TestDohDetachedLosingHedgeRetainsFinalResolverOutcome(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	log := newRecordingLogger()
	loserStarted := make(chan context.Context, 1)
	releaseLoser := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseLoser) }) }
	defer release()
	winnerReady := make(chan struct{})
	winnerDone := make(chan error, 1)
	settings := DefaultDohSettings()
	settings.Log = log
	settings.DohServerStagger = 0
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{"http://192.0.2.61/dns-query"},
		RemoteDohUrlsIpv6: []string{"http://[2001:db8::61]/dns-query"},
	}
	settings.DialContextSettings = &DialContextSettings{DialContext: func(dialCtx context.Context, network string, address string) (net.Conn, error) {
		if dohAttemptFamily(address) == "6" {
			loserStarted <- dialCtx
			select {
			case <-releaseLoser:
			case <-ctx.Done():
			}
			return nil, &net.OpError{Op: "dial", Net: network, Addr: &net.TCPAddr{IP: net.ParseIP("2001:db8::61"), Port: 80}, Err: os.ErrDeadlineExceeded}
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			select {
			case <-winnerReady:
			case <-ctx.Done():
				winnerDone <- ctx.Err()
				return
			}
			request, err := http.ReadRequest(bufio.NewReader(server))
			if err != nil {
				winnerDone <- err
				return
			}
			response := httptest.NewRecorder()
			writeDohWire(response, request, []netip.Addr{netip.MustParseAddr("198.51.100.61")}, 30, false)
			winnerDone <- response.Result().Write(server)
		}()
		return client, nil
	}}
	cache := NewDohCache(settings)
	defer cache.Close()
	result := make(chan bool, 1)
	go func() {
		addrs, authoritative := cache.QueryResult(ctx, "A", "private-query.example")
		result <- authoritative && len(addrs) == 1
	}()
	var loserCtx context.Context
	select {
	case loserCtx = <-loserStarted:
	case <-ctx.Done():
		t.Fatal("losing hedge did not start")
	}
	close(winnerReady)
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("winning resolver answer changed")
		}
	case <-ctx.Done():
		t.Fatal("winner waited for detached losing dial")
	}
	if loserCtx.Err() != nil {
		t.Fatalf("fixture failed to retain the real transport detached dial: %v", loserCtx.Err())
	}
	observation, ok := loserCtx.Value(dohResolverObservationKey{}).(*dohResolverObservation)
	if !ok || dohResolverOutcome(observation.outcome.Load()) != dohOutcomeAnswer {
		t.Fatal("terminal answer did not reach detached dial context")
	}
	release()
	cache.Close()
	if err := <-winnerDone; err != nil {
		t.Fatal(err)
	}
	lines := log.linesWith("attempted_family=6 result=timeout")
	if len(lines) != 1 || !strings.Contains(lines[0], "path=caller") || !strings.Contains(lines[0], "resolver_outcome=answer") {
		t.Fatalf("detached losing hedge was not attributed to its answered call: %v", lines)
	}
	for _, line := range log.linesWith("observable=v1") {
		for _, private := range []string{"192.0.2.61", "2001:db8::61", "198.51.100.61", "private-query.example"} {
			if strings.Contains(line, private) {
				t.Fatal("identity escaped in new observable schema")
			}
		}
	}
}

// The final cache outcome includes fallback and stale serving, not merely the
// remote DoH fanout outcome. No short negative timeout is used as evidence.
func TestDohResolverObservationIncludesFinalFallbackAndStale(t *testing.T) {
	for _, stale := range []bool{false, true} {
		settings := DefaultDohSettings()
		settings.Log = NewNoopLogger()
		settings.DnsResolverSettings = &DnsResolverSettings{
			EnableRemoteDoh:   true,
			EnableLocalDoh:    !stale,
			RemoteDohUrlsIpv4: []string{"https://192.0.2.62/dns-query"},
			LocalDohUrlsIpv4:  []string{"https://192.0.2.63/dns-query"},
		}
		cache := NewDohCache(settings)
		query := NewDohKey("A", "fallback.example")
		if stale {
			cache.queryResultExpiration[query] = &DohResult{
				Time:            time.Now().Add(-time.Minute),
				AddrExpirations: map[netip.Addr]time.Time{netip.MustParseAddr("198.51.100.62"): time.Now().Add(-time.Second)},
			}
		}
		var observation *dohResolverObservation
		cache.remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			observation = request.Context().Value(dohResolverObservationKey{}).(*dohResolverObservation)
			return nil, errors.New("synthetic remote failure")
		})
		cache.localClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			response := httptest.NewRecorder()
			writeDohWire(response, request, []netip.Addr{netip.MustParseAddr("198.51.100.63")}, 30, false)
			return response.Result(), nil
		})
		addrs, authoritative := cache.QueryResult(context.Background(), "A", "fallback.example")
		cache.Close()
		if !authoritative || len(addrs) != 1 || observation == nil {
			t.Fatalf("stale=%t final result changed", stale)
		}
		want := dohOutcomeAnswer
		if stale {
			want = dohOutcomeStale
		}
		if got := dohResolverOutcome(observation.outcome.Load()); got != want {
			t.Fatalf("stale=%t observed=%s want=%s", stale, got, want)
		}
	}
}

// Fixed counters count every terminal call while emission is throttled. A
// returned answer remains an answer when cancellation races its return.
func TestDohResolverObservationCounterAndTerminalKinds(t *testing.T) {
	var counter dohResolverCounter
	now := time.Unix(1000, 0)
	for outcome := dohOutcomeAnswer; outcome < dohOutcomeCount; outcome++ {
		counts, emit := counter.record(outcome, now)
		if counts[outcome] != 1 || emit != (outcome == dohOutcomeAnswer) {
			t.Fatalf("counter/emission changed for %s: %v %t", outcome, counts, emit)
		}
	}
	counts, emit := counter.record(dohOutcomeAnswer, now.Add(controlDialLogInterval))
	if !emit || counts[dohOutcomeAnswer] != 2 || counts[dohOutcomeFailed] != 1 || counts[dohOutcomeStale] != 1 {
		t.Fatalf("cumulative snapshot lost suppressed outcomes: %v %t", counts, emit)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, expire := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer expire()
	for _, test := range []struct {
		ctx           context.Context
		answered      bool
		authoritative bool
		stale         bool
		want          dohResolverOutcome
	}{
		{ctx: context.Background(), want: dohOutcomeFailed},
		{ctx: canceled, want: dohOutcomeCanceled},
		{ctx: expired, want: dohOutcomeTimeout},
		{ctx: canceled, answered: true, authoritative: true, want: dohOutcomeAnswer},
		{ctx: context.Background(), authoritative: true, want: dohOutcomeEmpty},
		{ctx: context.Background(), answered: true, authoritative: true, stale: true, want: dohOutcomeStale},
	} {
		if got := dohFinalResolverOutcome(test.ctx, test.answered, test.authoritative, test.stale); got != test.want {
			t.Errorf("terminal outcome=%s want=%s", got, test.want)
		}
	}
}

// A complete failure, authoritative empty result, and caller cancellation use
// distinct outcomes at the real cache boundary, after its own cleanup runs.
func TestDohResolverObservationEmptyFailureAndCancellation(t *testing.T) {
	for _, want := range []dohResolverOutcome{dohOutcomeEmpty, dohOutcomeFailed, dohOutcomeCanceled} {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultDohSettings()
		settings.Log = NewNoopLogger()
		settings.DnsResolverSettings = &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: []string{"https://192.0.2.64/dns-query"},
		}
		cache := NewDohCache(settings)
		var observation *dohResolverObservation
		cache.remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			observation = request.Context().Value(dohResolverObservationKey{}).(*dohResolverObservation)
			if want == dohOutcomeCanceled {
				cancel()
				return nil, context.Canceled
			}
			if want == dohOutcomeFailed {
				return nil, errors.New("synthetic transport failure")
			}
			response := httptest.NewRecorder()
			writeDohWire(response, request, nil, 30, true)
			return response.Result(), nil
		})
		addrs, authoritative := cache.QueryResult(ctx, "A", "missing.example")
		cache.Close()
		cancel()
		if len(addrs) != 0 || authoritative != (want == dohOutcomeEmpty) {
			t.Fatalf("%s changed the cache result", want)
		}
		if observation == nil || dohResolverOutcome(observation.outcome.Load()) != want {
			t.Fatalf("cache terminal outcome did not preserve %s", want)
		}
	}
}

// An opaque response and an external-client one-shot finish at their own
// boundary; the arbitrary external transport never acquires host provenance.
func TestDohResolverObservationForwardAndExternalOneShot(t *testing.T) {
	for _, forward := range []bool{true, false} {
		settings := DefaultDohSettings()
		settings.Log = NewNoopLogger()
		settings.DnsResolverSettings = &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: []string{"https://192.0.2.65/dns-query"},
		}
		var observation *dohResolverObservation
		transport := dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			observation = request.Context().Value(dohResolverObservationKey{}).(*dohResolverObservation)
			response := httptest.NewRecorder()
			writeDohWire(response, request, nil, 30, true)
			return response.Result(), nil
		})
		if forward {
			cache := NewDohCache(settings)
			cache.remoteClient.httpClient.Transport = transport
			_, usable := cache.Forward(context.Background(), dnsmessage.TypeTXT, "opaque.example")
			cache.Close()
			if !usable || observation == nil || observation.scope != dohScopeForward || dohResolverOutcome(observation.outcome.Load()) != dohOutcomeAnswer {
				t.Fatal("opaque usable response did not finish at the forward boundary")
			}
		} else {
			answers := DohQueryWithClient(context.Background(), &http.Client{Transport: transport}, 4, "A", settings, "missing.example")
			if len(answers) != 0 || observation == nil || observation.path != dohPathUnknown || observation.scope != dohScopeOneShot || dohResolverOutcome(observation.outcome.Load()) != dohOutcomeEmpty {
				t.Fatal("one-shot external transport acquired false path or outcome provenance")
			}
		}
	}
}
