package connect

// Expansion candidates belong to the custom configuration read before their
// construction. Explicit barriers keep replacement between that read and
// publication, including returned dialers consumed without another lookup.

import (
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// Builds one verified discovery candidate without opening any network path.
// Quic leaves the profile deterministic: tcp randomizes fragment and reorder.
func newCustomExtenderPublicationStrategy(t *testing.T, disableManual bool) (*ClientStrategy, netip.Addr) {
	t.Helper()
	clock := newTestClock()
	strategy, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, func(settings *ClientStrategySettings) {
		settings.EnableNormal = false
		settings.EnableResilient = false
		settings.DisableManualExtenders = disableManual
		settings.ExpandExtenderProfileCount = 3
		settings.MaxExtenderCount = 6
		settings.Log = NewNoopLogger()
	})
	ip := netip.MustParseAddr("192.0.2.41")
	record := signTestRecord(t, rootPrivateKey, newTestExtenderKey(t), clock.Now(), clock.Now().Add(24*time.Hour), testExtenderAddress(ip.String(), ExtenderCarrierQuic))
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	return strategy, ip
}

// Waits for a positive lifecycle event; timeout is only a failure bound.
func waitCustomExtenderPublication(t *testing.T, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(5 * time.Second):
		t.Fatal("custom extender publication did not reach its barrier")
	}
}

// The existing family-probe seam runs after the strategy snapshot and before
// directory candidates are constructed. Cleanup releases and joins the worker.
func blockCustomExtenderDiscovery(t *testing.T, strategy *ClientStrategy) func() []*clientDialer {
	t.Helper()
	entered, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	restore := swapControlFamilyProbe(func(family int) bool {
		if family == 4 {
			enterOnce.Do(func() { close(entered); <-release })
		}
		return family == 4
	})
	t.Cleanup(restore)
	t.Cleanup(func() { unblock(); waitCustomExtenderPublication(t, stopped) })
	result := make(chan []*clientDialer, 1)
	go func() {
		defer close(stopped)
		result <- strategy.expandExtenderDialers()
	}()
	waitCustomExtenderPublication(t, entered)
	return func() []*clientDialer {
		unblock()
		waitCustomExtenderPublication(t, stopped)
		return <-result
	}
}

// Checks both publication and the returned fast path used by parallelEval.
func assertCustomExtenderPublicationRefused(t *testing.T, strategy *ClientStrategy, expanded []*clientDialer) {
	t.Helper()
	if len(expanded) != 0 {
		t.Errorf("superseded expansion returned %d selectable dialers", len(expanded))
	}
	if dialers := testExtenderDialers(strategy); len(dialers) != 0 {
		t.Errorf("superseded expansion published %d dialers", len(dialers))
	}
	for dialer := range strategy.dialerWeights(false) {
		if dialer.IsExtender() {
			t.Errorf("superseded extender %s remained selectable", dialer.extenderConfig.Ip)
		}
	}
}

// A fresh expansion must still publish routes for the currently owned config.
func assertCustomExtenderPublicationCurrent(t *testing.T, strategy *ClientStrategy, expanded []*clientDialer, ip netip.Addr, secret string) {
	t.Helper()
	if len(expanded) == 0 {
		t.Fatal("current configuration produced no extender")
	}
	for _, dialer := range expanded {
		if dialer.extenderConfig.Ip != ip || dialer.extenderConfig.Secret != secret {
			t.Errorf("returned extender does not belong to the current configuration")
		}
	}
	for _, dialer := range testExtenderDialers(strategy) {
		if dialer.extenderConfig.Ip != ip || dialer.extenderConfig.Secret != secret {
			t.Errorf("published extender does not belong to the current configuration")
		}
	}
	for dialer := range strategy.dialerWeights(false) {
		if dialer.IsExtender() && (dialer.extenderConfig.Ip != ip || dialer.extenderConfig.Secret != secret) {
			t.Errorf("selectable extender does not belong to the current configuration")
		}
	}
}

// A discovery snapshot must not survive a switch to a custom-only route.
func TestClientStrategyCustomExtenderPublicationRejectsReplacement(t *testing.T) {
	strategy, _ := newCustomExtenderPublicationStrategy(t, false)
	finish := blockCustomExtenderDiscovery(t, strategy)
	ip := netip.MustParseAddr("198.51.100.42")
	strategy.SetCustomExtenders(map[netip.Addr]string{ip: "synthetic-replacement-secret"})
	assertCustomExtenderPublicationRefused(t, strategy, finish())
	assertCustomExtenderPublicationCurrent(t, strategy, strategy.expandExtenderDialers(), ip, "synthetic-replacement-secret")
}

// Replacing with the same value still resets existing dialers and fences
// pending work, even when there were no published dialers to remove yet.
func TestClientStrategyCustomExtenderPublicationRejectsSameConfigReset(t *testing.T) {
	strategy, ip := newCustomExtenderPublicationStrategy(t, false)
	finish := blockCustomExtenderDiscovery(t, strategy)
	strategy.SetCustomExtenders(nil)
	assertCustomExtenderPublicationRefused(t, strategy, finish())
	assertCustomExtenderPublicationCurrent(t, strategy, strategy.expandExtenderDialers(), ip, "")
}

// Returning to the same map does not restore ownership of an earlier snapshot.
func TestClientStrategyCustomExtenderPublicationRejectsChangeBack(t *testing.T) {
	strategy, ip := newCustomExtenderPublicationStrategy(t, false)
	finish := blockCustomExtenderDiscovery(t, strategy)
	strategy.SetCustomExtenders(map[netip.Addr]string{netip.MustParseAddr("198.51.100.43"): "synthetic-intermediate-secret"})
	strategy.SetCustomExtenders(nil)
	assertCustomExtenderPublicationRefused(t, strategy, finish())
	assertCustomExtenderPublicationCurrent(t, strategy, strategy.expandExtenderDialers(), ip, "")
}

// No configuration mutation means the captured expansion remains useful.
func TestClientStrategyCustomExtenderPublicationPreservesUnchanged(t *testing.T) {
	strategy, ip := newCustomExtenderPublicationStrategy(t, false)
	finish := blockCustomExtenderDiscovery(t, strategy)
	assertCustomExtenderPublicationCurrent(t, strategy, finish(), ip, "")
}

// A refused manual update must not invalidate the verified discovery snapshot.
func TestClientStrategyCustomExtenderPublicationPreservesDisabledUpdate(t *testing.T) {
	strategy, ip := newCustomExtenderPublicationStrategy(t, true)
	finish := blockCustomExtenderDiscovery(t, strategy)
	strategy.SetCustomExtenders(map[netip.Addr]string{netip.MustParseAddr("198.51.100.44"): "synthetic-disabled-secret"})
	assertCustomExtenderPublicationCurrent(t, strategy, finish(), ip, "")
}

// Closing during candidate construction must suppress the returned fast path.
func TestClientStrategyCustomExtenderPublicationRejectsClosed(t *testing.T) {
	strategy, _ := newCustomExtenderPublicationStrategy(t, false)
	finish := blockCustomExtenderDiscovery(t, strategy)
	strategy.Close()
	assertCustomExtenderPublicationRefused(t, strategy, finish())
}

// Retirement of an old country profile exposes the same snapshot boundary in
// the manual branch, which deliberately performs no family or directory probe.
type customExtenderPublicationTransport struct {
	closeIdle func()
}

// The fixture owns no network and must never be used for an HTTP exchange.
func (self *customExtenderPublicationTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("synthetic publication fixture cannot send requests")
}

// Lets the test replace configuration after the manual snapshot was captured.
func (self *customExtenderPublicationTransport) CloseIdleConnections() { self.closeIdle() }

// A manual address/secret replacement must reject the old manual construction.
func TestClientStrategyCustomExtenderPublicationRejectsManualReplacement(t *testing.T) {
	strategy, _ := newCustomExtenderPublicationStrategy(t, false)
	oldIp, newIp := netip.MustParseAddr("198.51.100.45"), netip.MustParseAddr("198.51.100.46")
	strategy.SetCustomExtenders(map[netip.Addr]string{oldIp: "synthetic-old-secret"})
	entered, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	transport := &customExtenderPublicationTransport{closeIdle: func() {
		enterOnce.Do(func() { close(entered); <-release })
	}}
	func() {
		strategy.mutex.Lock()
		defer strategy.mutex.Unlock()
		strategy.extenderSpoofCountryCode = "synthetic-old-country"
		strategy.dialers[&clientDialer{
			extenderConfig: &ExtenderConfig{Ip: oldIp},
			httpClient:     &http.Client{Transport: transport},
			settings:       strategy.settings,
		}] = true
	}()
	t.Cleanup(func() { unblock(); waitCustomExtenderPublication(t, stopped) })
	result := make(chan []*clientDialer, 1)
	go func() { defer close(stopped); result <- strategy.expandExtenderDialers() }()
	waitCustomExtenderPublication(t, entered)
	strategy.SetCustomExtenders(map[netip.Addr]string{newIp: "synthetic-new-secret"})
	unblock()
	waitCustomExtenderPublication(t, stopped)
	assertCustomExtenderPublicationRefused(t, strategy, <-result)
	assertCustomExtenderPublicationCurrent(t, strategy, strategy.expandExtenderDialers(), newIp, "synthetic-new-secret")
}

// Concurrent replacements all invalidate the suspended snapshot, and only
// the final explicit configuration can seed its successor expansion.
func TestClientStrategyCustomExtenderPublicationRejectsConcurrentReplacements(t *testing.T) {
	strategy, _ := newCustomExtenderPublicationStrategy(t, false)
	finish := blockCustomExtenderDiscovery(t, strategy)
	var replacements sync.WaitGroup
	for i := range 4 {
		replacements.Add(1)
		go func() {
			defer replacements.Done()
			ip := netip.AddrFrom4([4]byte{198, 51, 100, byte(60 + i)})
			strategy.SetCustomExtenders(map[netip.Addr]string{ip: "synthetic-concurrent-secret"})
		}()
	}
	replacements.Wait()
	ip := netip.MustParseAddr("203.0.113.47")
	strategy.SetCustomExtenders(map[netip.Addr]string{ip: "synthetic-final-secret"})
	assertCustomExtenderPublicationRefused(t, strategy, finish())
	assertCustomExtenderPublicationCurrent(t, strategy, strategy.expandExtenderDialers(), ip, "synthetic-final-secret")
}
