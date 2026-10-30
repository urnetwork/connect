// A deterministic composed recovery assertion. The existing private fixture
// closes a real reliable source gate, and the exact peer Ack reopens it.
package connect

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// A local refusal must remain quiet while readiness is unchanged, but cannot
// strand an already credited SYN when the refused source actually becomes ready.
func TestNativeSynRetryRefusedReplacementResumesAfterCapacity(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		credit := newNativeBatchCreditFixture(t)
		harness, path := synRetryControlHarness(t, 51307, nil, DefaultProtocolVersion)
		var sourceOffers, retryOffers atomic.Int32
		harness.multiClient.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if !reraceSameFlow(group.ipPath, path) || !group.ipPath.Syn || group.ipPath.Ack {
				return
			}
			if group.isSynRetry() {
				retryOffers.Add(1)
			} else {
				sourceOffers.Add(1)
			}
		}
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("initial native SYN refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		update.stateLock.Lock()
		ownerCredit, ownerSerial := owner.credit, owner.serial
		original := bytes.Clone(owner.packet)
		update.stateLock.Unlock()
		started := credit.base.started.Load()
		harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{credit.base.selected}
		}
		harness.inject("a", reraceProviderSignal(t, path))
		for range 3 {
			harness.inject("a", reraceProviderSignal(t, path))
		}
		synctest.Wait()
		if credit.base.started.Load() != started+1 || !credit.source.resendCapacityUnavailable.Load() {
			t.Fatal("premise: one actual retained retry did not refuse at the closed source gate")
		}
		if credit.base.selected.dialFailureCount() != 0 || credit.base.selected.IsDone() {
			t.Fatal("local refusal struck or destroyed the healthy replacement")
		}
		update.stateLock.Lock()
		quiet := update.synRetry == owner && owner.references == 1 && !owner.queued && update.client.Load() == nil && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		if !quiet || sourceOffers.Load() != 1 || retryOffers.Load() != 1 {
			t.Fatal("local refusal changed source ownership or lost the bounded original")
		}
		synctest.Wait()
		if credit.base.started.Load() != started+1 {
			t.Fatal("unchanged readiness spun an extra replacement attempt")
		}

		// This acknowledges the exact decoded retained seed. No capacity flag,
		// clock, packet source, provider signal or queue notification is faked.
		credit.releasePeerCredit()
		attempts := credit.base.started.Load()
		if attempts == started+1 {
			if credit.source.resendCapacityUnavailable.Load() {
				t.Fatal("premise: exact peer acknowledgement did not reopen real source capacity")
			}
			t.Fatal("RETAINED_SYN_READY_EDGE_LOST: exact capacity recovery did not reoffer the credited SYN once without a native retransmission")
		}
		if attempts != started+2 {
			t.Fatal("one real capacity opening duplicated the retained replacement offer")
		}
		// A successful original can fill the one-item window and reclose the
		// gate before quiescence. Its exact wire packet supplies positive proof.
		if harness.boundTo(path) != credit.base.selected {
			t.Fatal("capacity recovery did not bind the admitted replacement")
		}
		credit.finishPackets(original)
		synctest.Wait()
		if credit.base.started.Load() != started+2 || credit.base.selected.dialFailureCount() != 0 ||
			sourceOffers.Load() != 1 || retryOffers.Load() != 2 {
			t.Fatal("one real ready edge duplicated source admission or provider strikes")
		}
		update.stateLock.Lock()
		balanced := update.synRetry == owner && owner.references == 1 && !owner.queued &&
			owner.credit == ownerCredit && owner.serial == ownerSerial && bytes.Equal(owner.packet, original) &&
			update.client.Load() == credit.base.selected && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		if !balanced || ownerCredit.released.Load() || harness.update(path) != update {
			t.Fatal("capacity recovery changed canonical source custody or retained an extra dispatcher reference")
		}
		harness.multiClient.stateLock.Lock()
		booked := harness.multiClient.clientUpdates[credit.base.selected][update]
		harness.multiClient.stateLock.Unlock()
		if !booked {
			t.Fatal("the admitted replacement was missing from actual flow bookkeeping")
		}
		if err := harness.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synRetryControlReleased(t, owner)
		if !ownerCredit.released.Load() {
			t.Fatal("retiring the resumed flow retained its final credited source bytes")
		}
	})
}
