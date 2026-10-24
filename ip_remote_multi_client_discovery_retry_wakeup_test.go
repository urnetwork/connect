// Private source-only regression: a cleared discovery backoff must release
// the already-waiting production enumerator without advancing virtual time.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Reuses the existing discovery fixture and empty listing counter. The real
// enumerator is durably blocked on its retry wait before this helper returns.
func privateDeferredDiscoveryEnumerator(t *testing.T) (*multiClientWindow, *listingCountDiscoveryGenerator, time.Time, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	window := discoveryRetryTestWindow(t)
	window.ctx = ctx
	window.cancel = cancel
	window.networkChangeMonitor = NewMonitor()
	window.clientChannelArgs = make(chan *multiClientChannelArgs)
	generator := &listingCountDiscoveryGenerator{}
	window.generator = generator
	window.noteDiscoveryClientCount(1)
	start := time.Now()
	window.noteDiscoveryExpandUnproductive(start)
	if _, deferred := window.discoveryExpandDeferral(start); !deferred {
		t.Fatal("unqualified root failure: discovery retry was not armed")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		window.randomEnumerateClientArgs()
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		window.evalEpochCancel()
	})
	synctest.Wait()
	if !time.Now().Equal(start) || generator.listings() != 0 {
		t.Fatal("unqualified root failure: enumerator did not park before retry expiry")
	}
	return window, generator, start, done
}

// Resize excludes retained warned clients from its selectable count. Its
// existing expansion wake must make the cleared state visible to a producer
// already waiting on the old deadline, while established clients remain owned.
func TestPrivateDiscoveryRetrySelectableExitLossWakesEnumerator(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		window, generator, start, _ := privateDeferredDiscoveryEnumerator(t)
		window.noteDiscoveryClientCount(0)
		if _, deferred := window.discoveryExpandDeferral(start); deferred {
			t.Fatal("unqualified root failure: selectable exit loss did not clear retry state")
		}
		// The production expand request emits this exact wake. It currently
		// cannot release the retry wait that encloses provider discovery.
		window.generatorMonitor.NotifyAll()
		synctest.Wait()
		if !time.Now().Equal(start) {
			t.Fatal("unqualified root failure: recovery consumed the old retry timer")
		}
		t.Logf("qualified retry reset: selectable count=1->0 deferred=false virtual_elapsed=%s listings=%d", time.Since(start), generator.listings())
		if generator.listings() == 0 {
			t.Fatal("cleared discovery retry left enumerator asleep after selectable exit loss")
		}
	})
}

// Cancellation must join the same deferred producer without listing, minting,
// waiting for its retry timer, or relying on an active expansion consumer.
func TestPrivateDiscoveryRetryDeferredEnumeratorCancellationJoins(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		window, generator, start, done := privateDeferredDiscoveryEnumerator(t)
		window.cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("cancellation left deferred discovery enumerator running")
		}
		if !time.Now().Equal(start) || generator.listings() != 0 {
			t.Fatal("cancellation consumed the retry timer or listed providers")
		}
	})
}
