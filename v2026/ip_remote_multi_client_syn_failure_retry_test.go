// An explicit failure arriving after an accepted native SYN must not leave
// recovery waiting for another native retransmission when a healthy exit exists.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// This uses the real public native/Client/Transfer path and real provider ICMP
// parser. Only the test's failure delivery order is controlled; no native retry,
// helper send, replacement selection or changed timeout supplies the second SYN.
func TestNativeExplicitFailureRetriesOwnedSynWithoutNextOffer(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := reraceFlow(4, 0, 51301)
		harness := newSilenceInferenceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1, answer: func(candidate *IpPath) reraceAnswer {
				if reraceSameFlow(candidate, path) {
					return reraceSilent
				}
				return reraceAccept
			}},
		}, nil)
		a, b := harness.channel("a"), harness.channel("b")
		if harness.multiClient.sendClientPathForTest != nil || a.sendGroupForTest != nil || b.sendGroupForTest != nil {
			t.Fatal("fixture bypasses real native routing or source custody")
		}
		nativeOffers := 1
		if !privateStaleBoundNativeSyn(t, harness, path) || harness.boundTo(path) != a ||
			len(harness.exit("a").syns(path)) != 1 || len(harness.exit("b").syns(path)) != 0 {
			t.Fatal("original native SYN did not acquire exactly A's real source")
		}
		update := harness.update(path)
		// Ordering only: this is not a new timeout or a latency threshold.
		time.Sleep(time.Nanosecond)
		harness.inject("a", reraceProviderSignal(t, path))
		synctest.Wait()
		if harness.metrics().DialFailuresIntercepted != 1 || a.dialFailureCount() != 1 ||
			update.receivedInbound.Load() || harness.update(path) != update {
			t.Fatal("the exact live provider failure was not observed before any answer")
		}
		if count := len(harness.exit("b").syns(path)); count != 1 {
			t.Errorf("EXPLICIT_FAILURE_WAITED_FOR_NEXT_NATIVE_SYN: B_wire_syns=%d binding_nil=%t native_offers=%d",
				count, update.client.Load() == nil, nativeOffers)
			return
		}
		update.stateLock.Lock()
		current := update.client.loadSelection()
		owned := current != nil && current.client == b && update.synWaitSelection == current &&
			update.synWaitClient == b && update.synWaitSynNumber == path.SequenceNumber && !update.synWaitStart.IsZero()
		update.stateLock.Unlock()
		if !owned || len(harness.exit("a").syns(path)) != 1 || nativeOffers != 1 {
			t.Fatal("recovery lacked fresh B custody or manufactured another native offer")
		}
		harness.inject("b", reraceSynAck(path))
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != b || harness.update(path) != update {
			t.Fatal("owned SYN retry did not establish B on the original flow")
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}
