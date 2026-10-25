// A selected send snapshot may become stale before the group gate.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func privateStaleBoundNativeSyn(t *testing.T, harness *reraceHarness, path *IpPath) bool {
	t.Helper()
	packet := MessagePoolCopy(reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil))
	accepted := [1]bool{}
	count := harness.multiClient.SendNativePacketBatchWithResults(harness.source, protocol.ProvideMode_Network,
		[][]byte{packet}, 5*time.Second, accepted[:])
	harness.settle()
	if count < 0 || count > 1 || (count == 1) != accepted[0] {
		t.Fatalf("native stale-gate disposition count=%d accepted=%t", count, accepted[0])
	}
	return accepted[0]
}

// The real affinity predicate qualifies A. Only after that decision, a provider
// ICMP travels real Client/Transfer return ownership and unbinds A. This same
// native SYN must reach B's real source rather than wait for another app retry.
func TestNativeStaleBoundGateExplicitFailureReachesFreshSource(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := reraceFlow(4, 0, 51201)
		harness := newSilenceInferenceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1, answer: func(candidate *IpPath) reraceAnswer {
				if reraceSameFlow(candidate, path) {
					return reraceSilent
				}
				return reraceAccept
			}},
		}, nil)
		privateStaleBoundHoldSynRetry(t, harness)
		a, b := harness.channel("a"), harness.channel("b")
		if harness.multiClient.sendClientPathForTest != nil || a.sendGroupForTest != nil || b.sendGroupForTest != nil {
			t.Fatal("fixture bypasses actual affinity or grouped source admission")
		}
		if !privateStaleBoundNativeSyn(t, harness, path) || harness.boundTo(path) != a ||
			len(harness.exit("a").syns(path)) != 1 {
			t.Fatal("first native SYN did not acquire actual A custody")
		}
		update := harness.update(path)
		requireSilenceInferenceAffinity(t, harness, path)
		oldSelection := update.client.loadSelection()
		entered, qualified, published := 0, false, false
		harness.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate,
			snapshot *multiClientChannel, group *parsedPacketGroup) {
			if !reraceSameFlow(group.ipPath, path) {
				return
			}
			entered++
			qualified = current == update && snapshot == a && group.affinitySynReuse &&
				current.client.loadSelection() == oldSelection
			harness.inject("a", reraceProviderSignal(t, path))
			published = current.client.Load() == nil && harness.metrics().DialFailuresIntercepted == 1 &&
				a.dialFailureCount() == 1
		}
		accepted := privateStaleBoundNativeSyn(t, harness, path)
		harness.multiClient.beforePacketGroupGateForTest = nil
		if entered != 1 || !qualified || !published {
			t.Fatalf("exact stale-snapshot interleaving missing: entered=%d real_affinity=%t actual_failure=%t",
				entered, qualified, published)
		}
		if !accepted || len(harness.exit("b").syns(path)) != 1 {
			t.Errorf("STALE_BOUND_SYN_GATE_DISCARDED_WITHDRAWN_SOURCE: accepted=%t B_wire_syns=%d binding_nil=%t",
				accepted, len(harness.exit("b").syns(path)), update.client.Load() == nil)
			return
		}
		update.stateLock.Lock()
		clockCurrent := !update.receivedInbound.Load() && !update.synWaitStart.IsZero() && update.synWaitClient == b &&
			update.synWaitSelection == update.client.loadSelection() && update.synWaitSynNumber == path.SequenceNumber
		update.stateLock.Unlock()
		if !clockCurrent || len(harness.exit("a").syns(path)) != 1 {
			t.Fatal("fresh source borrowed A's accepted clock or retransmitted to failed A")
		}
		harness.inject("b", reraceSynAck(path))
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != b || harness.update(path) != update {
			t.Fatal("fresh native custody did not deliver B's SYNACK on the original flow")
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// Hold the independent automatic retry while these tests pin the native
// sender's gate interleaving. The worker owns no parent/flow lock at this seam.
func privateStaleBoundHoldSynRetry(t *testing.T, harness *reraceHarness) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	harness.multiClient.beforeSynRetryDispatchForTest = func(*multiClientChannelUpdate) {
		select {
		case <-release:
		case <-harness.multiClient.ctx.Done():
		}
	}
}
