package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Every control below retains actual public parsing, affinity and source owners.
func privateStaleGateFixture(t *testing.T, port int) (*reraceHarness, *IpPath, *multiClientChannel, *multiClientChannel) {
	t.Helper()
	path := reraceFlow(4, 0, port)
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
	if !privateStaleBoundNativeSyn(t, harness, path) || harness.boundTo(path) != a || len(harness.exit("a").syns(path)) != 1 {
		t.Fatal("adjacent fixture lacks initial native A custody")
	}
	requireSilenceInferenceAffinity(t, harness, path)
	return harness, path, a, b
}

func privateStaleGateRequireB(t *testing.T, harness *reraceHarness, path *IpPath, b *multiClientChannel) {
	t.Helper()
	if len(harness.exit("b").syns(path)) != 1 || harness.boundTo(path) != b {
		t.Fatal("current B did not acquire one real source SYN")
	}
	update := harness.update(path)
	update.stateLock.Lock()
	valid := !update.receivedInbound.Load() && update.synWaitClient == b && !update.synWaitStart.IsZero() &&
		update.synWaitSelection == update.client.loadSelection()
	update.stateLock.Unlock()
	if !valid {
		t.Fatal("B custody lacked its exact unanswered admission clock")
	}
	harness.inject("b", reraceSynAck(path))
	if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != b {
		t.Fatal("B answer did not establish its owned source")
	}
	reraceAssertNoErrorDelivered(t, harness, path)
}

// A concurrent owner rebind must not send using A paired with B's receipt.
func TestNativeStaleBoundGateRebindUsesCurrentB(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, path, a, b := privateStaleGateFixture(t, 51202)
		update := h.update(path)
		entered := 0
		h.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate, snapshot *multiClientChannel, group *parsedPacketGroup) {
			if !reraceSameFlow(group.ipPath, path) {
				return
			}
			entered++
			if snapshot != a || !group.affinitySynReuse {
				t.Error("rebind missed actual A affinity snapshot")
			}
			current.stateLock.Lock()
			current.selectAffinityClient(b)
			current.stateLock.Unlock()
			h.multiClient.bindClientFlow(current, b)
		}
		accepted := privateStaleBoundNativeSyn(t, h, path)
		h.multiClient.beforePacketGroupGateForTest = nil
		if entered != 1 || !accepted || h.update(path) != update || len(h.exit("a").syns(path)) != 1 {
			t.Fatal("changed current owner did not govern this same native offer")
		}
		privateStaleGateRequireB(t, h, path, b)
	})
}

// A late answer can commit while a sender holds nil. The newly current owner's
// retained SYN remains covered; the refresh must not duplicate it.
func TestNativeStaleBoundGateNilSnapshotKeepsLateAnswerCoverage(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, path, a, _ := privateStaleGateFixture(t, 51203)
		update := h.update(path)
		if !h.multiClient.clientDialFailure(a, path, false) || update.client.Load() != nil {
			t.Fatal("late-answer control did not enter the existing inferred-unbind state")
		}
		entered, answered := 0, false
		h.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate, snapshot *multiClientChannel, group *parsedPacketGroup) {
			if !reraceSameFlow(group.ipPath, path) {
				return
			}
			entered++
			if snapshot != nil {
				t.Error("late-answer sender did not capture nil")
			}
			h.inject("a", reraceSynAck(path))
			answered = current.client.Load() == a && current.receivedInbound.Load()
		}
		accepted := privateStaleBoundNativeSyn(t, h, path)
		h.multiClient.beforePacketGroupGateForTest = nil
		if entered != 1 || !answered || accepted || update.client.Load() != a || len(h.exit("a").syns(path)) != 1 {
			t.Fatal("stale nil snapshot duplicated or displaced a late-established source")
		}
		reraceAssertNoErrorDelivered(t, h, path)
	})
}

// Pointer equality alone must not overwrite the new immutable A receipt or
// let its generation borrow the earlier accepted clock after A->nil->A.
func TestNativeStaleBoundGateSelectionAbaPreservesReceipt(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, path, a, _ := privateStaleGateFixture(t, 51204)
		update := h.update(path)
		original := update.client.loadSelection()
		update.stateLock.Lock()
		clock := update.synWaitStart
		update.stateLock.Unlock()
		var replacement *multiClientAffinitySelection
		h.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate, snapshot *multiClientChannel, group *parsedPacketGroup) {
			if !reraceSameFlow(group.ipPath, path) {
				return
			}
			current.stateLock.Lock()
			current.selectAffinityClient(nil)
			current.selectAffinityClient(a)
			replacement = current.client.loadSelection()
			current.stateLock.Unlock()
		}
		accepted := privateStaleBoundNativeSyn(t, h, path)
		h.multiClient.beforePacketGroupGateForTest = nil
		update.stateLock.Lock()
		preserved := update.client.loadSelection() == replacement && replacement != original &&
			update.synWaitSelection == original && update.synWaitStart.Equal(clock) && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		if accepted || !preserved || len(h.exit("a").syns(path)) != 1 || len(h.exit("b").syns(path)) != 0 {
			t.Fatal("selection ABA changed retained coverage, current receipt or accepted-clock authority")
		}
	})
}

// A real first answer defeats a late provider error. Refreshed gating retains
// the established source and never manufactures a replacement clock.
func TestNativeStaleBoundGateEstablishedAnswerSurvivesSignal(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, path, a, _ := privateStaleGateFixture(t, 51205)
		update := h.update(path)
		selection := update.client.loadSelection()
		h.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate, snapshot *multiClientChannel, group *parsedPacketGroup) {
			if !reraceSameFlow(group.ipPath, path) {
				return
			}
			h.inject("a", reraceSynAck(path))
			h.inject("a", reraceProviderSignal(t, path))
		}
		accepted := privateStaleBoundNativeSyn(t, h, path)
		h.multiClient.beforePacketGroupGateForTest = nil
		if accepted || !update.receivedInbound.Load() || update.client.loadSelection() != selection ||
			a.dialFailureCount() != 0 || len(h.exit("a").syns(path)) != 1 || len(h.exit("b").syns(path)) != 0 {
			t.Fatal("late signal disturbed or duplicated a real established source")
		}
		reraceAssertNoErrorDelivered(t, h, path)
	})
}

// A current-selection refresh is only an offer. A real closed reliable-credit
// gate must refuse B without spending its peer credit or advancing this SYN.
func TestNativeStaleBoundGateRefusedFreshSourceKeepsOwnership(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		h, path, _, _ := privateStaleGateFixture(t, 51206)
		update := h.update(path)
		update.stateLock.Lock()
		epoch, clock := update.sequenceAdmissionEpoch, update.synWaitStart
		update.stateLock.Unlock()
		started := credit.base.started.Load()
		h.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{credit.base.selected}
		}
		h.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate, snapshot *multiClientChannel, group *parsedPacketGroup) {
			if reraceSameFlow(group.ipPath, path) {
				h.inject("a", reraceProviderSignal(t, path))
			}
		}
		accepted := privateStaleBoundNativeSyn(t, h, path)
		h.multiClient.beforePacketGroupGateForTest = nil
		update.stateLock.Lock()
		unchanged := update.client.Load() == nil && update.sequenceAdmissionEpoch == epoch &&
			update.synWaitStart.Equal(clock) && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		count, size := credit.source.resendQueue.QueueSize()
		if accepted || !unchanged || credit.base.started.Load() != started+1 || count != 1 || size <= 1 ||
			!credit.source.resendCapacityUnavailable.Load() || credit.base.selected.dialFailureCount() != 0 {
			t.Fatal("refused replacement skipped its real gate or acquired ownership/clock/credit/strike")
		}
	})
}

// The same receipt check is needed after a positive gate but before actual
// source admission. One new TFO byte makes this retry eligible on A; a real
// failure at the existing before-completion seam must redirect this same offer.
func TestNativeStaleBoundAdmissionReceiptReachesFreshSource(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, path, a, b := privateStaleGateFixture(t, 51207)
		entered := 0
		a.beforeGroupCompletionForTest = func() {
			entered++
			h.inject("a", reraceProviderSignal(t, path))
		}
		packet := MessagePoolCopy(reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, []byte{0x71}))
		accepted := [1]bool{}
		count := h.multiClient.SendNativePacketBatchWithResults(h.source, protocol.ProvideMode_Network, [][]byte{packet}, 5*time.Second, accepted[:])
		h.settle()
		a.beforeGroupCompletionForTest = nil
		if entered != 1 || count != 1 || !accepted[0] || len(h.exit("a").syns(path)) != 1 || len(h.exit("b").syns(path)) != 1 {
			t.Errorf("STALE_BOUND_ADMISSION_RECEIPT_SENT_WITHDRAWN_SOURCE: barrier=%d count=%d accepted=%t A_syns=%d B_syns=%d",
				entered, count, accepted[0], len(h.exit("a").syns(path)), len(h.exit("b").syns(path)))
			return
		}
		wire := h.exit("b").syns(path)[0]
		if len(wire.payload) != 1 || wire.payload[0] != 0x71 {
			t.Fatal("replacement source lost exact TFO payload")
		}
		privateStaleGateRequireB(t, h, path, b)
	})
}
