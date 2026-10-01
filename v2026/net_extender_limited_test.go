package connect

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

// A limited extender on the client (EXTENDER.md A12): a 429 is an
// ExtenderLimitedError and never a failure; the directory leaves the address
// alone for a jittered backoff and orders it after every healthy one; the
// probe pass, the feed dial and the peer pinger skip it and record nothing of
// it; and a dialer with nothing but limited extenders waits for the first
// backoff rather than dialing again.

// A tls responder on loopback that answers every extender request with one
// status and header, and the config that dials it.
func newTestStatusExtenderResponder(t *testing.T, statusCode int, header http.Header) *ExtenderConfig {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.ReadAll(req.Body)
		for name, values := range header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(statusCode)
	}))
	t.Cleanup(server.Close)
	addrPort := netip.MustParseAddrPort(server.Listener.Addr().(*net.TCPAddr).String())
	return &ExtenderConfig{
		Profile: ExtenderProfile{
			ConnectMode: ExtenderConnectModeTcpTls,
			Port:        int(addrPort.Port()),
		},
		Ip: addrPort.Addr(),
	}
}

// A 429 is a limit with the Retry-After it carried; another refusal is still a
// refusal.
func TestDialExtenderReadsA429AsALimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dial := func(extenderConfig *ExtenderConfig) error {
		conn, _, err := DialExtender(ctx, DefaultConnectSettings(), extenderConfig, &ExtenderDial{
			DestinationHost: "dest.example",
			DestinationPort: 443,
		})
		if conn != nil {
			conn.Close()
		}
		return err
	}

	err := dial(newTestStatusExtenderResponder(t, http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}}))
	var limitedErr *ExtenderLimitedError
	if !errors.As(err, &limitedErr) || limitedErr.RetryAfter != 30*time.Second {
		t.Fatalf("a 429 got %v, expected a limit of 30s", err)
	}
	var refusedErr *ExtenderRefusedError
	if errors.As(err, &refusedErr) {
		t.Fatal("a 429 read as a refusal")
	}

	err = dial(newTestStatusExtenderResponder(t, http.StatusTooManyRequests, nil))
	if !errors.As(err, &limitedErr) || limitedErr.RetryAfter != 0 {
		t.Fatalf("a 429 without Retry-After got %v", err)
	}

	err = dial(newTestStatusExtenderResponder(t, http.StatusForbidden, http.Header{"Retry-After": {"30"}}))
	if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden || errors.As(err, &limitedErr) {
		t.Fatalf("a 403 got %v, expected a refusal", err)
	}
}

// Retry-After in either form, and nothing for what is not one.
func TestExtenderRetryAfterParsesBothForms(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		value  string
		expect time.Duration
	}{
		{value: "30", expect: 30 * time.Second},
		{value: " 15 ", expect: 15 * time.Second},
		{value: "0", expect: 0},
		{value: "-5", expect: 0},
		{value: "soon", expect: 0},
		{value: "", expect: 0},
		{value: "99999999999", expect: 24 * time.Hour},
		{value: now.Add(45 * time.Second).Format(http.TimeFormat), expect: 45 * time.Second},
		{value: now.Add(-time.Minute).Format(http.TimeFormat), expect: 0},
	}
	for _, c := range cases {
		header := http.Header{}
		if c.value != "" {
			header.Set("Retry-After", c.value)
		}
		if retryAfter := extenderRetryAfter(header, now); retryAfter != c.expect {
			t.Errorf("%q: %s, expected %s", c.value, retryAfter, c.expect)
		}
	}
}

// The backoff is the Retry-After, or the default without one, jittered within
// half either way.
func TestJitterExtenderLimitedBackoffStaysWithinHalf(t *testing.T) {
	low, high := time.Duration(0), time.Duration(0)
	for i := 0; i < 2000; i += 1 {
		backoff := JitterExtenderLimitedBackoff(30*time.Second, time.Hour)
		if backoff < 15*time.Second || 45*time.Second < backoff {
			t.Fatalf("backoff %s is outside 15s to 45s", backoff)
		}
		if low == 0 || backoff < low {
			low = backoff
		}
		high = max(high, backoff)
	}
	// the draws spread over the range rather than sitting on the value
	if 20*time.Second < low || high < 40*time.Second {
		t.Fatalf("2000 draws spanned only %s to %s", low, high)
	}
	for i := 0; i < 100; i += 1 {
		if backoff := JitterExtenderLimitedBackoff(0, 30*time.Second); backoff < 15*time.Second || 45*time.Second < backoff {
			t.Fatalf("the default backoff drew %s", backoff)
		}
	}
	if backoff := JitterExtenderLimitedBackoff(0, 0); backoff != 0 {
		t.Fatalf("no backoff at all drew %s", backoff)
	}
}

// A limited address is not a failure: no failure count, no hold, still usable,
// and never removed for it. It orders after every healthy address, the hinted
// continent's included, for its jittered backoff; the probe pass leaves it
// out; and once the backoff passes it is back in its place.
func TestExtenderDirectoryOrdersALimitedAddressLast(t *testing.T) {
	directory, clock := newTestProximityDirectory(t)
	directory.SetContinentHint("EU")
	limitedIp := netip.MustParseAddr("192.0.2.1")
	if ips := candidateIps(directory.Candidates(4, 10)); !slices.Equal(ips, []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}) {
		t.Fatalf("order before the limit = %v", ips)
	}

	directory.RecordLimited(limitedIp, 30*time.Second)
	limitedUntil := directory.AddressLimitedUntil(limitedIp)
	if limitedUntil.Before(clock.Now().Add(15*time.Second)) || clock.Now().Add(45*time.Second).Before(limitedUntil) {
		t.Fatalf("limited until %s, expected 15s to 45s from %s", limitedUntil, clock.Now())
	}
	candidates := directory.Candidates(4, 10)
	if ips := candidateIps(candidates); !slices.Equal(ips, []string{"192.0.2.2", "192.0.2.3", "192.0.2.1"}) {
		t.Fatalf("order during the limit = %v", ips)
	}
	if !candidates[2].LimitedUntil.Equal(limitedUntil) || !candidates[0].LimitedUntil.IsZero() {
		t.Fatal("the candidates do not say which is limited")
	}
	if ips := candidateIps(directory.ProbeCandidates(4, 10, false)); slices.Contains(ips, "192.0.2.1") {
		t.Fatalf("the probe pass would measure a limited address: %v", ips)
	}
	if !directory.AddressUsable(limitedIp) {
		t.Fatal("a limited address is not usable")
	}
	for _, entry := range directory.Snapshot().Entries {
		if entry.Ip != limitedIp {
			continue
		}
		if entry.FailureCount != 0 || entry.State == ExtenderStateHold || entry.State == ExtenderStateWarning {
			t.Fatalf("a limited address was judged a failure: %+v", entry)
		}
		if !entry.LimitedUntil.Equal(limitedUntil) {
			t.Fatalf("the status shows limited until %s", entry.LimitedUntil)
		}
	}

	// a shorter backoff never shortens a longer one
	directory.RecordLimited(limitedIp, time.Second)
	if !directory.AddressLimitedUntil(limitedIp).Equal(limitedUntil) {
		t.Fatal("a shorter backoff shortened the limit")
	}

	clock.advance(limitedUntil.Sub(clock.Now()) + time.Second)
	if !directory.AddressLimitedUntil(limitedIp).IsZero() {
		t.Fatal("the limit did not pass")
	}
	if ips := candidateIps(directory.Candidates(4, 10)); !slices.Equal(ips, []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}) {
		t.Fatalf("order after the limit = %v", ips)
	}

	// no Retry-After takes the configured backoff, jittered the same way
	directory.RecordLimited(limitedIp, 0)
	if until := directory.AddressLimitedUntil(limitedIp); until.Before(clock.Now().Add(15*time.Second)) || clock.Now().Add(45*time.Second).Before(until) {
		t.Fatalf("the default backoff limited until %s", until)
	}
}

// The carrier walk the probe pass and the peer pinger share records a limited
// probe as a limit and nothing else: no failure, no other carrier tried.
func TestProbeExtenderCandidateRecordsALimitAndNoFailure(t *testing.T) {
	directory, _ := newTestProximityDirectory(t)
	candidate := directory.Candidates(4, 1)[0]
	probeCount := 0
	_, err := probeExtenderCandidate(
		context.Background(),
		directory,
		candidate,
		2,
		5*time.Second,
		func(ctx context.Context, extenderConfig *ExtenderConfig) (*ExtenderLatencyProbe, error) {
			probeCount += 1
			return nil, &ExtenderLimitedError{RetryAfter: 20 * time.Second}
		},
	)
	var limitedErr *ExtenderLimitedError
	if !errors.As(err, &limitedErr) {
		t.Fatalf("the walk got %v, expected the limit", err)
	}
	if probeCount != 1 {
		t.Fatalf("the walk probed %d times past a limit", probeCount)
	}
	if directory.AddressLimitedUntil(candidate.Ip).IsZero() {
		t.Fatal("the address was not limited")
	}
	for _, entry := range directory.Snapshot().Entries {
		if entry.Ip == candidate.Ip && (entry.FailureCount != 0 || entry.Latency != 0) {
			t.Fatalf("a limited probe was recorded: %+v", entry)
		}
	}
}

// A strategy whose only dialer is a limited extender dials nothing until the
// backoff passes, whatever its reconnect pace, and dials again as soon as it
// has; the limit is not counted as an error of the dialer, and a sibling
// dialer of the same address is limited with it.
func TestClientStrategyWaitsOutALimitedExtender(t *testing.T) {
	clock := newTestClock()
	// the strategy's own limits are on the wall clock, so the directory's are
	// too here
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.Now = time.Now
	})
	extenderPublicKey := newTestExtenderKey(t)
	record := signTestRecord(
		t,
		rootPrivateKey,
		extenderPublicKey,
		time.Now(),
		time.Now().Add(24*time.Hour),
		testExtenderAddress("192.0.2.9"),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParseAddr("192.0.2.9")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientStrategySettings()
	settings.EnableNormal, settings.EnableResilient = false, false
	settings.ExtenderDirectory = directory
	settings.ReconnectTimeout = 10 * time.Millisecond
	settings.ExtenderConfigs = []*ExtenderConfig{
		{Profile: ExtenderProfile{ConnectMode: ExtenderConnectModeTcpTls, Port: 443}, Ip: ip},
		{Profile: ExtenderProfile{ConnectMode: ExtenderConnectModeQuic, Port: 443}, Ip: ip},
	}
	strategy := NewClientStrategy(ctx, settings)
	defer strategy.Close()
	dialers := slices.Collect(func(yield func(*clientDialer) bool) {
		for dialer := range strategy.dialerWeights(false) {
			if !yield(dialer) {
				return
			}
		}
	})
	if len(dialers) != 2 {
		t.Fatalf("%d extender dialers", len(dialers))
	}

	// the limit, as a dial reports it
	dialers[0].Update(ctx, &ExtenderLimitedError{RetryAfter: 600 * time.Millisecond})
	if weight := dialers[0].Weight(); weight != dialers[0].minimumWeight {
		t.Fatalf("a limit counted against the dialer: weight %f", weight)
	}
	dialers[0].mutex.Lock()
	errorCount := dialers[0].errorCount
	dialers[0].mutex.Unlock()
	if errorCount != 0 {
		t.Fatalf("a limit counted %d errors", errorCount)
	}
	if weights := strategy.dialerWeights(false); 0 < len(weights) {
		t.Fatalf("%d limited dialers are still weighed, the sibling of the address included", len(weights))
	}
	limitedUntil := dialers[0].LimitedUntil()
	if limitedUntil.IsZero() {
		t.Fatal("the dialer is not limited")
	}

	var evalLock sync.Mutex
	evalTimes := []time.Time{}
	evalCtx, evalCancel := context.WithTimeout(ctx, 3*time.Second)
	defer evalCancel()
	result := strategy.parallelEval(evalCtx, false, func(ctx context.Context, dialer *clientDialer) *evalResult {
		evalLock.Lock()
		evalTimes = append(evalTimes, time.Now())
		evalLock.Unlock()
		return &evalResult{err: errors.New("no destination in this test")}
	})
	if result != nil && result.err == nil {
		t.Fatal("the eval selected a dialer")
	}
	evalLock.Lock()
	defer evalLock.Unlock()
	if len(evalTimes) == 0 {
		t.Fatal("the dialers were never dialed once the backoff passed")
	}
	if evalTimes[0].Before(limitedUntil) {
		t.Fatalf("a limited dialer was dialed %s before its backoff passed", limitedUntil.Sub(evalTimes[0]))
	}
}

// A manual extender, which the directory does not know, keeps its backoff on
// its dialer: the Retry-After jittered within half either way, the dialer left
// out of the weights until then, and nothing recorded in the directory.
func TestClientStrategyLimitsAManualExtenderOnItsDialer(t *testing.T) {
	for _, withDirectory := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		ip := netip.MustParseAddr("192.0.2.77")
		settings := DefaultClientStrategySettings()
		settings.EnableNormal, settings.EnableResilient = false, false
		if withDirectory {
			directory, _ := newTestExtenderDirectory(t, newTestClock(), func(settings *ExtenderDirectorySettings) {
				settings.Now = time.Now
			})
			settings.ExtenderDirectory = directory
		}
		settings.ExtenderConfigs = []*ExtenderConfig{
			{Profile: ExtenderProfile{ConnectMode: ExtenderConnectModeTcpTls, Port: 443}, Ip: ip},
		}
		strategy := NewClientStrategy(ctx, settings)
		var dialer *clientDialer
		for weighed := range strategy.dialerWeights(false) {
			dialer = weighed
		}
		if dialer == nil {
			t.Fatalf("directory %t: no extender dialer", withDirectory)
		}

		before := time.Now()
		dialer.Update(ctx, &ExtenderLimitedError{RetryAfter: 10 * time.Second})
		after := time.Now()
		limitedUntil := dialer.LimitedUntil()
		if limitedUntil.Before(before.Add(5*time.Second)) || after.Add(15*time.Second).Before(limitedUntil) {
			t.Fatalf("directory %t: limited for %s, expected within [5s, 15s]", withDirectory, limitedUntil.Sub(before))
		}
		if weights, weightsLimitedUntil := strategy.dialerWeightsUnlimited(false); 0 < len(weights) || !weightsLimitedUntil.Equal(limitedUntil) {
			t.Fatalf(
				"directory %t: %d dialers weighed and limited until %s, expected none until %s",
				withDirectory,
				len(weights),
				weightsLimitedUntil,
				limitedUntil,
			)
		}
		if withDirectory {
			if addressLimitedUntil := settings.ExtenderDirectory.AddressLimitedUntil(ip); !addressLimitedUntil.IsZero() {
				t.Fatalf("the directory recorded an address it does not know until %s", addressLimitedUntil)
			}
		}
		strategy.Close()
		cancel()
	}
}

// A network client over a directory the test has already filled, with no
// probe pass and nothing resolved for real, counting its passes by the
// synchronous TXT bootstrap every pass makes below the low-water mark.
// Address resolution may still be pending when the pass reaches its wait.
func newTestLimitedNetworkClient(
	t *testing.T,
	clock *testClock,
	directory *ExtenderDirectory,
	configures ...func(settings *ExtenderNetworkClientSettings),
) (*ExtenderNetworkClient, func() int) {
	t.Helper()
	var resolveLock sync.Mutex
	resolveCount := 0
	settings := DefaultExtenderNetworkClientSettings()
	settings.Now = clock.Now
	settings.ExtenderDnsName = "extender.space.example"
	settings.MinBackoff = time.Millisecond
	settings.MaxBackoff = 10 * time.Millisecond
	settings.DialTimeout = 2 * time.Second
	settings.HelloTimeout = 2 * time.Second
	settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
	settings.ProbeWindowCount = 0
	settings.LowWaterCount = 16
	settings.Hello = func(ctx context.Context) (*ExtenderHelloResult, error) {
		return nil, nil
	}
	settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
		resolveLock.Lock()
		defer resolveLock.Unlock()
		resolveCount += 1
		return nil, nil
	}
	settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
		return nil, nil
	}
	for _, configure := range configures {
		configure(settings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	networkClient := NewExtenderNetworkClient(ctx, newTestDeadDialStrategy(t, ctx), directory, settings)
	t.Cleanup(func() {
		networkClient.Close()
		cancel()
	})
	return networkClient, func() int {
		resolveLock.Lock()
		defer resolveLock.Unlock()
		return resolveCount
	}
}

// The feed dial skips a limited candidate and records nothing against it,
// dialing the healthy one.
func TestExtenderNetworkClientFeedSkipsALimitedCandidate(t *testing.T) {
	directory, clock := newTestProximityDirectory(t)
	limitedIp := netip.MustParseAddr("192.0.2.1")
	directory.RecordLimited(limitedIp, time.Hour)
	newTestLimitedNetworkClient(t, clock, directory)

	// the dead dial fails every healthy candidate, which is what shows it was
	// dialed; the limited one is never dialed, so it has no failure. Every
	// failure is a directory change, so the check runs on each one.
	deadline := time.After(10 * time.Second)
	for {
		_, change := directory.ChangeMonitor().Get()
		failedIps := map[netip.Addr]bool{}
		for _, entry := range directory.Snapshot().Entries {
			if 0 < entry.FailureCount {
				failedIps[entry.Ip] = true
			}
		}
		if failedIps[limitedIp] {
			t.Fatal("the feed dialed a limited candidate")
		}
		if failedIps[netip.MustParseAddr("192.0.2.2")] {
			break
		}
		select {
		case <-change:
		case <-deadline:
			t.Fatal("the feed never dialed the healthy candidates")
		}
	}
}

// With every candidate limited the feed dials nothing and passes again only
// once the first backoff has passed, not on its reconnect backoff.
func TestExtenderNetworkClientWaitsWhenEveryCandidateIsLimited(t *testing.T) {
	directory, clock := newTestProximityDirectory(t)
	for _, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"} {
		directory.RecordLimited(netip.MustParseAddr(ip), time.Hour)
	}
	// every wait the loop chooses between passes, which holds it: nothing
	// fires them, so no pass follows the one that found every candidate
	// limited
	waits := make(chan time.Duration, 16)
	// Keep address resolution behind the pass assertion. Existing candidates
	// let the feed reach its wait while DNS is pending, so resolver execution
	// cannot serve as a pass counter. Cleanup releases the held worker.
	releaseDns := make(chan struct{})
	defer close(releaseDns)
	networkClient, passCount := newTestLimitedNetworkClient(t, clock, directory, func(settings *ExtenderNetworkClientSettings) {
		resolveDns := settings.ResolveDns
		settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
			select {
			case <-releaseDns:
				return resolveDns(ctx, name)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		settings.PassAfter = func(wait time.Duration) <-chan time.Time {
			waits <- wait
			return make(chan time.Time)
		}
	})
	select {
	case wait := <-waits:
		// the limits were recorded jittered from an hour, so the first of
		// them passes no sooner than half an hour out, where the client's own
		// backoff is milliseconds
		if wait < 30*time.Minute {
			t.Fatalf("the client waits %s with every candidate limited, expected the first backoff", wait)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the client never finished a pass")
	}
	if status := networkClient.Status(); status.LastError != "every extender candidate is limited" {
		t.Fatalf("status = %+v", status)
	}
	if count := passCount(); count != 1 {
		t.Fatalf("%d passes before the first wait, expected one", count)
	}
	for _, entry := range directory.Snapshot().Entries {
		if 0 < entry.FailureCount {
			t.Fatalf("a limited candidate was dialed: %+v", entry)
		}
	}
}

// A peer that answers 429 is recorded nowhere -- no ping, no report, no
// sample -- and is due again when its limit passes, not a refresh away.
func TestExtenderPeerPingerRecordsNoVerdictForALimitedPeer(t *testing.T) {
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	peerPublicKey := newTestExtenderKey(t)
	record := signTestRecord(
		t,
		rootPrivateKey,
		peerPublicKey,
		clock.Now(),
		clock.Now().Add(24*time.Hour),
		testExtenderAddress("192.0.2.20", ExtenderCarrierTcp),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	peerIp := netip.MustParseAddr("192.0.2.20")

	pinged := make(chan struct{}, 8)
	settings := DefaultExtenderPeerPingerSettings()
	settings.Now = clock.Now
	settings.SpreadTimeout = 0
	settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
	settings.Ping = func(ctx context.Context, extenderConfig *ExtenderConfig, attestor *ExtenderProbeAttestor) (*ExtenderLatencyProbe, error) {
		pinged <- struct{}{}
		return nil, &ExtenderLimitedError{RetryAfter: 20 * time.Second}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pinger := NewExtenderPeerPinger(ctx, nil, directory, settings)
	defer pinger.Close()

	select {
	case <-pinged:
	case <-time.After(10 * time.Second):
		t.Fatal("the peer was never pinged")
	}
	keyHex := hex.EncodeToString(peerPublicKey)
	// the end of every ping notifies the ping end monitor, so the check runs
	// on each one
	waitForTestPingEnd := func() *extenderPeerPingerPeer {
		deadline := time.After(10 * time.Second)
		for {
			pingEnd := pinger.pingEndMonitor.NotifyChannel()
			pinger.stateLock.Lock()
			peer := pinger.peers[keyHex]
			var copied extenderPeerPingerPeer
			if peer != nil {
				copied = *peer
			}
			pinger.stateLock.Unlock()
			if peer != nil && !copied.pinging {
				return &copied
			}
			select {
			case <-pingEnd:
			case <-deadline:
				t.Fatal("the ping did not end")
			}
		}
	}
	peer := waitForTestPingEnd()
	limitedUntil := directory.AddressLimitedUntil(peerIp)
	if limitedUntil.IsZero() {
		t.Fatal("the peer's address was not limited")
	}
	if !peer.dueTime.Equal(limitedUntil) {
		t.Fatalf("the peer is due %s, expected when its limit passes, %s", peer.dueTime, limitedUntil)
	}
	status := pinger.Status()
	if status.PingCount != 0 || status.FailedCount != 0 || status.RejectedCount != 0 || status.UnknownCount != 0 {
		t.Fatalf("a limited ping was recorded: %+v", status)
	}
	if records := pinger.Records(); 0 < len(records) {
		t.Fatalf("%d ring records for a limited ping", len(records))
	}
	for _, entry := range directory.Snapshot().Entries {
		if entry.Ip == peerIp && (entry.FailureCount != 0 || entry.Latency != 0) {
			t.Fatalf("a limited ping was recorded in the directory: %+v", entry)
		}
	}
}
