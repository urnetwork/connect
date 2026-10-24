// Each retry clear wakes the real discovery producer. Subscription and clear
// ordering are forced separately without timers, sleeps or scheduling guesses.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Uses the frozen root fixture; only the event that clears retry state varies.
func privateCheckDiscoveryRetryClearWakesEnumerator(t *testing.T, clear func(*multiClientWindow)) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		window, generator, start, _ := privateDeferredDiscoveryEnumerator(t)
		clear(window)
		if _, deferred := window.discoveryExpandDeferral(start); deferred {
			t.Fatal("clear left discovery retry armed")
		}
		synctest.Wait()
		if !time.Now().Equal(start) || generator.listings() == 0 {
			t.Fatal("clear left production enumerator waiting for its old retry timer")
		}
	})
}

// A successful provider admission must release a producer already deferred.
func TestPrivateDiscoveryRetryAdmissionWakesEnumerator(t *testing.T) {
	privateCheckDiscoveryRetryClearWakesEnumerator(t, func(window *multiClientWindow) {
		window.noteDiscoveryExpandAdmitted()
	})
}

// An explicit state clear has the same wake contract as provider admission.
func TestPrivateDiscoveryRetryExplicitClearWakesEnumerator(t *testing.T) {
	privateCheckDiscoveryRetryClearWakesEnumerator(t, func(window *multiClientWindow) {
		window.clearDiscoveryExpandRetry()
	})
}

// Network changes retain the existing immediate release of discovery waits.
func TestPrivateDiscoveryRetryNetworkChangeWakesEnumerator(t *testing.T) {
	privateCheckDiscoveryRetryClearWakesEnumerator(t, func(window *multiClientWindow) {
		window.notifyNetworkChanged()
	})
}

// Repeated clears must not emit fresh edges for unchanged state, and every
// later unproductive pass must retain its full deferral until another clear.
func TestPrivateDiscoveryRetryRepeatedClearPreservesNextWait(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		window, generator, start, _ := privateDeferredDiscoveryEnumerator(t)
		for round := range 3 {
			if round != 0 {
				window.noteDiscoveryExpandUnproductive(start)
				window.generatorMonitor.NotifyAll()
				synctest.Wait()
				if generator.listings() != round {
					t.Fatalf("round %d bypassed an unproductive pass's deferral", round)
				}
			}
			window.clearDiscoveryExpandRetry()
			changed := window.resizeMonitor.NotifyChannel()
			window.clearDiscoveryExpandRetry()
			window.noteDiscoveryExpandAdmitted()
			select {
			case <-changed:
				t.Fatalf("round %d emitted a wake for unchanged cleared retry state", round)
			default:
			}
			synctest.Wait()
			if !time.Now().Equal(start) || generator.listings() != round+1 {
				t.Fatalf("round %d did not release exactly one deferred listing without a timer", round)
			}
		}
	})
}

// Force the clear after the predicate snapshot and before select. The exact
// production snapshot must carry the edge, including after a prior clear.
func TestPrivateDiscoveryRetryClearBetweenSnapshotAndWait(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		window := discoveryRetryTestWindow(t)
		defer window.evalEpochCancel()
		now := time.Now()
		for round := range 3 {
			window.noteDiscoveryExpandUnproductive(now)
			var changed <-chan struct{}
			_, deferred := window.discoveryExpandDeferral(now, &changed)
			if !deferred || changed == nil {
				t.Fatalf("round %d did not arm the production retry snapshot", round)
			}
			window.clearDiscoveryExpandRetry()
			select {
			case <-changed:
			default:
				t.Fatalf("round %d lost a clear between retry snapshot and wait", round)
			}
			_, deferred = window.discoveryExpandDeferral(now, &changed)
			if deferred {
				t.Fatalf("round %d did not read cleared retry state", round)
			}
			window.clearDiscoveryExpandRetry()
			select {
			case <-changed:
				t.Fatalf("round %d reused a consumed edge or woke an unchanged clear", round)
			default:
			}
		}
	})
}
