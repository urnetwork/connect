package connect

// Regressions for the review of the preset mechanisms (ADBLOCK3RD connect,
// 2026-11-09): the health generation never goes back, the failover prober
// never spins without work, the connect window closes for a query already
// waiting its handicap and stays shut without a predicate, a query's first
// failed attempt counts at once, the failover resolver is tunnel DoH only, the
// host fallback warms only in the connect window, a fallback win is no
// failure, and a failure counts only if the tunnel is still ready.

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// A tunnel resolver transport that never answers: each request waits for its
// own end.
func testNeverAnsweringTransport() http.RoundTripper {
	return dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
}

// Two generation resets finishing out of order (two settings swaps or network
// changes racing) leave the health on the newer generation, so its evidence
// still counts.
func TestResolverHealthResetNeverGoesBackAGeneration(t *testing.T) {
	mux, _ := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.ResolverFailingMinCount = 1
		settings.Dns.ResolverFailingMinDuration = 0
	})
	mux.SetTunnelReady(func() bool { return true })
	older := mux.markTunnelDohUnproven()
	newer := mux.markTunnelDohUnproven()
	// the older call's reset lands last
	mux.resetResolverHealth(older)
	mux.recordResolverEvidence(newer, mux.healthNow(), true, false, dohLookupResult{})
	if got := mux.DnsResolverHealth(); got.ConsecutiveFailureCount != 1 || got.State != DnsResolverHealthFailing {
		t.Fatalf("health = %+v after a failure of the current generation, expected failing: a stale reset took the health back a generation", got)
	}
}

// Concurrent generation changes always end with the health on the newest
// generation.
func TestResolverHealthConcurrentResetsEndOnTheNewestGeneration(t *testing.T) {
	mux := &UpgradeMux{}
	for i := range 2000 {
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				mux.markTunnelDohUnproven()
			}()
		}
		wg.Wait()
		mux.resolverHealthLock.Lock()
		healthGeneration := mux.resolverHealth.generation
		mux.resolverHealthLock.Unlock()
		if current := mux.tunnelDohGeneration.Load(); healthGeneration != current {
			t.Fatalf("iteration %d: health on generation %d, the mux on %d", i, healthGeneration, current)
		}
	}
}

// Failed over with a non-positive FailoverProbeInterval the prober has nothing
// left to do: it stops and nothing restarts it.
func TestFailoverProberHasNoWorkWithoutAProbeInterval(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Failover = &DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{testFailoverDohUrl}}
		settings.Dns.ResolverFailingMinCount = 1
		settings.Dns.ResolverFailingMinDuration = 0
		settings.Dns.FailoverProbeInterval = 0
		settings.Dns.ResolveTimeout = 300 * time.Millisecond
	})
	newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohPolicyAnswer{}, false
	}).installPrimary(mux)
	newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohNoError(-1, netip.MustParseAddr("198.51.100.131")), true
	}).installFailover(t, mux)
	var warms atomic.Int32
	warm := mux.failoverWarmFunction
	mux.failoverWarmFunction = func(ctx context.Context, cache *DohCache) bool {
		warms.Add(1)
		return warm(ctx, cache)
	}
	mux.SetTunnelReady(func() bool { return true })

	testPresetQuery(t, mux, rec, "t1.example", dnsmessage.TypeA, 0x1701)
	if !waitForCondition(5*time.Second, testPresetHealthIs(mux, DnsResolverHealthFailedOver)) {
		t.Fatalf("health = %+v, expected failedOver", mux.DnsResolverHealth())
	}
	if mux.failoverProbeNeeded() {
		t.Fatal("failed over without a probe interval, the prober still reports work: it would restart itself forever")
	}
	if !waitForCondition(5*time.Second, func() bool { return !mux.failoverProberRunning.Load() }) {
		t.Fatal("the failover prober kept running with nothing to do")
	}
	if got := warms.Load(); got != 1 {
		t.Fatalf("%d warm queries, expected the failover's proof alone", got)
	}
}

// Without a warm function (a hand-wired mux) the prober can do nothing, so it
// never starts.
func TestFailoverProberHasNoWorkWithoutAWarmFunction(t *testing.T) {
	mux, _ := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Failover = &DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{testFailoverDohUrl}}
		settings.Dns.ResolverFailingMinCount = 1
		settings.Dns.ResolverFailingMinDuration = 0
	})
	mux.failoverWarmFunction = nil
	mux.SetTunnelReady(func() bool { return true })
	mux.recordResolverEvidence(mux.tunnelDohGeneration.Load(), mux.healthNow(), true, false, dohLookupResult{})
	if got := mux.DnsResolverHealth(); got.State != DnsResolverHealthFailing {
		t.Fatalf("health = %+v, expected failing", got)
	}
	if mux.failoverProbeNeeded() {
		t.Fatal("without a warm function the prober reports work it cannot do: it would restart itself forever")
	}
	if !waitForCondition(5*time.Second, func() bool { return !mux.failoverProberRunning.Load() }) {
		t.Fatal("the failover prober kept running without a warm function")
	}
}

// A query that armed the host fallback while the connect window was open
// never asks it once the window has closed, on the address and the forward
// path alike.
func TestUpgradeMuxFallbackWindowClosedDuringTheHandicap(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Fallback = &DnsResolverSettings{EnableLocalDoh: true, LocalDohUrlsIpv4: []string{testFallbackDohUrl}}
		settings.Dns.FallbackUntilTunnelReady = true
		settings.Dns.LocalFallbackTimeout = time.Second
		settings.Dns.ColdLocalFallbackTimeout = time.Second
		settings.Dns.ResolveTimeout = 3 * time.Second
	})
	mux.mux.Tun().DohCache().remoteClient.httpClient.Transport = testNeverAnsweringTransport()
	fallback := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohNoError(-1, netip.MustParseAddr("198.51.100.132")), true
	})
	mux.fallbackDohCache.Load().localClient.httpClient.Transport = dohRoundTripperFunc(fallback.roundTrip)
	var ready atomic.Bool
	mux.SetTunnelReady(ready.Load)

	// both pipelines start, and arm the fallback, while the window is open
	testPresetSend(t, mux, "edge.example", dnsmessage.TypeA, 0x1801)
	testPresetSend(t, mux, "edge-https.example", dnsTypeHttps, 0x1802)
	// the tunnel becomes ready long before the one second handicap ends
	ready.Store(true)

	if reply := testPresetAwait(t, mux, rec, "edge.example", dnsmessage.TypeA, 0x1801); reply != nil {
		t.Fatalf("the host fallback answered after the connect window closed: %x", reply)
	}
	reply := testPresetAwait(t, mux, rec, "edge-https.example", dnsTypeHttps, 0x1802)
	if rcode, _ := testPresetReplyAnswers(t, reply); rcode != dnsmessage.RCodeServerFailure {
		t.Fatalf("forwarded reply = %v, expected SERVFAIL from the tunnel alone", rcode)
	}
	if asks := fallback.asks(); 0 < len(asks) {
		t.Fatalf("the host fallback was asked %v after the connect window closed", asks)
	}
}

// With FallbackUntilTunnelReady and no tunnel-ready predicate the window can
// never be seen open, so the host fallback never races: the flag fails closed.
func TestUpgradeMuxConnectWindowWithoutAPredicateNeverRacesTheFallback(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Fallback = &DnsResolverSettings{EnableLocalDoh: true, LocalDohUrlsIpv4: []string{testFallbackDohUrl}}
		settings.Dns.FallbackUntilTunnelReady = true
		settings.Dns.LocalFallbackTimeout = 50 * time.Millisecond
		settings.Dns.ColdLocalFallbackTimeout = 10 * time.Millisecond
		settings.Dns.ResolveTimeout = time.Second
	})
	mux.mux.Tun().DohCache().remoteClient.httpClient.Transport = testNeverAnsweringTransport()
	fallback := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohNoError(-1, netip.MustParseAddr("198.51.100.133")), true
	})
	mux.fallbackDohCache.Load().localClient.httpClient.Transport = dohRoundTripperFunc(fallback.roundTrip)

	if reply := testPresetQuery(t, mux, rec, "nopredicate.example", dnsmessage.TypeA, 0x1901); reply != nil {
		t.Fatalf("the host fallback answered with no tunnel-ready predicate: %x", reply)
	}
	if asks := fallback.asks(); 0 < len(asks) {
		t.Fatalf("the host fallback was asked %v with no tunnel-ready predicate", asks)
	}
}

// A query's first failed attempt is its failure, counted as soon as it fails,
// not when the retries run out: a black-holed preset trips the thresholds at
// its first dial timeout. The query's later failure does not count again.
func TestUpgradeMuxFirstFailedAttemptCountsAtOnce(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.ResolverFailingMinCount = 1
		settings.Dns.ResolverFailingMinDuration = 0
		settings.Dns.ResolveTimeout = 5 * time.Second
	})
	var requests atomic.Int32
	mux.mux.Tun().DohCache().remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return nil, errors.New("synthetic first attempt failure")
		}
		// the retry stands for a slow failure that ends with the query
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	mux.SetTunnelReady(func() bool { return true })

	testPresetSend(t, mux, "slow.example", dnsmessage.TypeA, 0x1a01)
	if !waitForCondition(3*time.Second, func() bool { return mux.DnsResolverHealth().ConsecutiveFailureCount == 1 }) {
		t.Fatalf("health = %+v while the query still retries: its first failed attempt did not count", mux.DnsResolverHealth())
	}
	if testPresetFlightRetired(mux, testPresetKey("slow.example", dnsmessage.TypeA)) {
		t.Fatal("the query ended before its retry did")
	}
	if got := mux.DnsResolverHealth(); got.State != DnsResolverHealthFailing {
		t.Fatalf("health = %+v, expected failing past the thresholds", got)
	}
	if reply := testPresetAwait(t, mux, rec, "slow.example", dnsmessage.TypeA, 0x1a01); reply != nil {
		t.Fatal("a failing resolver answered")
	}
	if got := mux.DnsResolverHealth(); got.ConsecutiveFailureCount != 1 {
		t.Fatalf("health = %+v: one query counted more than one failure", got)
	}
}

// The failover resolver is tunnel DoH only, whatever settings it is given: no
// local DoH, no plain dns of either kind, on a copy that leaves the caller's
// settings alone.
func TestUpgradeMuxFailoverIsTunnelDohOnly(t *testing.T) {
	failoverSettings := &DnsResolverSettings{
		EnableRemoteDoh:   true,
		EnableLocalDoh:    true,
		EnableRemoteDns:   true,
		EnableLocalDns:    true,
		RemoteDohUrlsIpv4: []string{testFailoverDohUrl},
		LocalDohUrlsIpv4:  []string{testFallbackDohUrl},
		RemoteDnsIpv4:     []string{"192.0.2.53"},
		LocalDnsIpv4:      []string{"192.0.2.54"},
	}
	mux, _ := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Failover = failoverSettings
		settings.Dns.ResolveTimeout = 300 * time.Millisecond
	})
	failover := mux.failoverDohCache.Load()
	if failover == nil {
		t.Fatal("no failover resolver")
	}
	rs := failover.settings.DnsResolverSettings
	if !rs.EnableRemoteDoh || rs.EnableLocalDoh || rs.EnableRemoteDns || rs.EnableLocalDns {
		t.Fatalf("failover modes remote doh %t, local doh %t, remote dns %t, local dns %t: expected remote DoH alone", rs.EnableRemoteDoh, rs.EnableLocalDoh, rs.EnableRemoteDns, rs.EnableLocalDns)
	}
	if 0 < len(rs.LocalDohUrlsIpv4)+len(rs.LocalDohUrlsIpv6)+len(rs.RemoteDnsIpv4)+len(rs.RemoteDnsIpv6)+len(rs.LocalDnsIpv4)+len(rs.LocalDnsIpv6) {
		t.Fatalf("failover keeps host or plain dns servers: %+v", rs)
	}
	if !failoverSettings.EnableLocalDoh || !failoverSettings.EnableRemoteDns || len(failoverSettings.LocalDnsIpv4) != 1 {
		t.Fatalf("the caller's failover settings were changed: %+v", failoverSettings)
	}

	// failing remote DoH: nothing falls through to the host or plain dns
	localDns := newFakeDnsServer(t, map[string]netip.Addr{"x.example": netip.MustParseAddr("192.0.2.99")})
	defer localDns.close()
	failover.localResolver = localDns.resolver()
	var localRequests atomic.Int32
	failover.remoteClient.httpClient.Transport = dohRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("synthetic resolver failure")
	})
	failover.localClient.httpClient.Transport = dohRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		localRequests.Add(1)
		return nil, errors.New("synthetic host path")
	})
	if addrs, authoritative := failover.QueryResult(context.Background(), "A", "x.example"); 0 < len(addrs) || authoritative {
		t.Fatalf("failover lookup = %v (authoritative %t), expected a failure", addrs, authoritative)
	}
	if localRequests.Load() != 0 || localDns.requests.Load() != 0 {
		t.Fatalf("the failover took the host path: %d DoH requests, %d plain dns queries", localRequests.Load(), localDns.requests.Load())
	}
}

// The host fallback warms only while the connect window is open: once the
// tunnel is ready a network change or a settings swap warms nothing over the
// host network.
func TestUpgradeMuxFallbackWarmOnlyInTheConnectWindow(t *testing.T) {
	newMux := func() (*UpgradeMux, *atomic.Int32) {
		mux, _ := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
			settings.Dns.Fallback = &DnsResolverSettings{EnableLocalDoh: true, LocalDohUrlsIpv4: []string{testFallbackDohUrl}}
			settings.Dns.FallbackUntilTunnelReady = true
		})
		var warms atomic.Int32
		mux.fallbackDohWarmFunction = func(context.Context, *DohCache, int) bool {
			warms.Add(1)
			return true
		}
		// keep the tunnel's own cold prober off every network
		mux.tunnelDohWarmFunction = func(context.Context, int) bool { return true }
		return mux, &warms
	}
	warmed := func(mux *UpgradeMux, warms *atomic.Int32, want int32) {
		t.Helper()
		if !waitForCondition(5*time.Second, func() bool {
			return warms.Load() == want && !mux.fallbackDohWarmerRunning.Load()
		}) {
			t.Fatalf("%d fallback warms, expected %d", warms.Load(), want)
		}
	}

	mux, warms := newMux()
	var ready atomic.Bool
	mux.SetTunnelReady(ready.Load)
	mux.WarmDns()
	warmed(mux, warms, 1)

	ready.Store(true)
	mux.NetworkChanged()
	settings := DefaultUpgradeMuxSettings()
	settings.Dns.Resolver = &DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{testPresetDohUrl}}
	settings.Dns.Fallback = &DnsResolverSettings{EnableLocalDoh: true, LocalDohUrlsIpv4: []string{testFallbackDohUrl}}
	settings.Dns.FallbackUntilTunnelReady = true
	mux.SetSettings(settings)
	warmed(mux, warms, 1)

	// with no predicate the window is never open
	unwired, unwiredWarms := newMux()
	unwired.WarmDns()
	warmed(unwired, unwiredWarms, 0)
}

// A fallback win cancels the tunnel's resolution; that is no failure of the
// tunnel resolver, end to end.
func TestUpgradeMuxFallbackWinIsNoFailure(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Fallback = &DnsResolverSettings{EnableLocalDoh: true, LocalDohUrlsIpv4: []string{testFallbackDohUrl}}
		settings.Dns.LocalFallbackTimeout = 50 * time.Millisecond
		settings.Dns.ColdLocalFallbackTimeout = 10 * time.Millisecond
		settings.Dns.ResolveTimeout = 2 * time.Second
		settings.Dns.ResolverFailingMinCount = 1
		settings.Dns.ResolverFailingMinDuration = 0
	})
	mux.mux.Tun().DohCache().remoteClient.httpClient.Transport = testNeverAnsweringTransport()
	fallbackAddr := netip.MustParseAddr("198.51.100.134")
	fallback := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohNoError(-1, fallbackAddr), true
	})
	mux.fallbackDohCache.Load().localClient.httpClient.Transport = dohRoundTripperFunc(fallback.roundTrip)
	mux.SetTunnelReady(func() bool { return true })

	reply := testPresetQuery(t, mux, rec, "won.example", dnsmessage.TypeA, 0x1b01)
	if _, addrs := testPresetReplyAnswers(t, reply); len(addrs) != 1 || addrs[0] != fallbackAddr {
		t.Fatalf("reply = %v, expected the fallback's answer", addrs)
	}
	// the flight retires only after the canceled tunnel worker has exited
	if !waitForCondition(5*time.Second, func() bool {
		return testPresetFlightRetired(mux, testPresetKey("won.example", dnsmessage.TypeA))
	}) {
		t.Fatal("the flight was not retired")
	}
	if got := mux.DnsResolverHealth(); got.ConsecutiveFailureCount != 0 || got.State == DnsResolverHealthFailing {
		t.Fatalf("health = %+v: a fallback win counted as a tunnel failure", got)
	}
}

// A failure counts only if the tunnel is still ready when it fails: a
// resolution that started while ready and failed after the tunnel went down
// is not the resolver's fault.
func TestUpgradeMuxFailureCountsOnlyIfTheTunnelIsStillReady(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.ResolverFailingMinCount = 1
		settings.Dns.ResolverFailingMinDuration = 0
		settings.Dns.ResolveTimeout = 2 * time.Second
	})
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var requests atomic.Int32
	mux.mux.Tun().DohCache().remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			started <- struct{}{}
			select {
			case <-release:
			case <-request.Context().Done():
			}
		}
		return nil, errors.New("synthetic resolver failure")
	})
	var ready atomic.Bool
	ready.Store(true)
	mux.SetTunnelReady(ready.Load)

	testPresetSend(t, mux, "down.example", dnsmessage.TypeA, 0x1c01)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the resolution never started")
	}
	// the tunnel goes down while the attempt is in flight, then it fails
	ready.Store(false)
	close(release)
	if reply := testPresetAwait(t, mux, rec, "down.example", dnsmessage.TypeA, 0x1c01); reply != nil {
		t.Fatal("a failing resolver answered")
	}
	if got := mux.DnsResolverHealth(); got.ConsecutiveFailureCount != 0 || got.State != DnsResolverHealthCold {
		t.Fatalf("health = %+v: a failure after the tunnel went down counted", got)
	}
}

// A health change made before the notifier goroutine first runs still reaches
// the callbacks: every mux's health starts cold, so the notifier counts cold
// as delivered rather than whatever state it happens to read when scheduled.
func TestResolverHealthCallbacksDeliverAChangeMadeBeforeTheNotifierRan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mux := &UpgradeMux{
		ctx:                        ctx,
		resolverHealthStateMonitor: NewMonitorValue[DnsResolverHealthState](DnsResolverHealthCold),
		resolverHealthCallbacks:    NewCallbackList[DnsResolverHealthFunction](),
	}
	mux.markTunnelDohUnproven()
	delivered := make(chan DnsResolverHealthState, 4)
	mux.AddDnsResolverHealthCallback(func(health DnsResolverHealth) {
		delivered <- health.State
	})
	// the resolver answers before the notifier has run at all
	mux.updateResolverHealth(func(health *resolverHealthState) {
		health.success(time.Now())
	})
	go mux.runResolverHealthCallbacks()
	select {
	case state := <-delivered:
		if state != DnsResolverHealthOk {
			t.Fatalf("delivered %s, expected ok", state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a change made before the notifier ran was never delivered")
	}
}
