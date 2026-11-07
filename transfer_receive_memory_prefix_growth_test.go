package connect

import (
	"testing"
	"testing/synctest"
)

// A real small SACK promise funds only part of the larger missing original.
// Repeated fresh decodes must recover downstream custody from later tentative
// owners without preempting the committed item or losing either paid portion.
func TestReceiveMemoryProviderFreshPrefixDecodeCanRecoverDownstreamCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiveProviderPressureFixtureWithPrefix(t, true)
		prefix := f.sequence.receiveQueue.GetBySequenceNumber(2)
		loan := f.sequence.holdPromise.reservedByteCount
		f.releaseForeign(f.decodeBytes - loan)
		if loan <= 0 || loan >= f.decodeBytes || f.root.Available() != f.decodeBytes-loan ||
			f.sequence.memoryPressure != (receiveMemoryPressure{}) {
			t.Fatal("mixed-prefix fixture did not isolate the actual smaller promise and fresh decoded remainder")
		}
		before := f.receiver.ReceiveStats()
		f.forward(f.originals[1], f.receiverIn)
		stalled := !f.atNat()
		if stalled {
			for range 3 {
				f.retryHead()
				if f.atNat() {
					stalled = false
					break
				}
			}
		}
		if stalled {
			if f.ackedCount != 1 || f.root.Available() != f.decodeBytes-loan ||
				f.sequence.holdPromise.reservedByteCount != loan || f.sequence.holdPromise.ingressLoanByteCount != 0 ||
				f.sequence.receiveQueue.GetBySequenceNumber(2) != prefix || !prefix.committed ||
				f.sequence.receiveQueue.GetBySequenceNumber(10) == nil || f.sequence.receiveQueue.GetBySequenceNumber(11) == nil ||
				f.sequence.memoryPressure != (receiveMemoryPressure{}) || f.sequence.ctx.Err() != nil ||
				f.receiver.ReceiveStats().ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount {
				t.Fatal("mixed-prefix stall changed its actual committed custody, ownership or source premise")
			}
			t.Logf("[rx-prefix-fresh-stall] decode=%d prefix=%d downstream=%d reclaimable=%d retries=3", f.decodeBytes, loan, f.deliveryBytes-f.decodeBytes, 2*f.deliveryBytes)
			// Explicit external-capacity rescue proves source and final TCP
			// custody still work; the historical failure remains reported.
			f.releaseForeign(f.deliveryBytes - f.decodeBytes)
			f.retryHead()
		}
		if f.receiver.ReceiveStats().ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount || f.ackCounts[2] != 0 {
			t.Fatal("mixed-prefix recovery preempted committed custody")
		}
		f.proveNatCustody()
		f.drain()
		f.close()
		if stalled {
			t.Error("RX_PROVIDER_FRESH_PREFIX_GROWTH_STALLED: a smaller actual SACK promise and fresh decoded remainder cannot recover downstream custody from later uncommitted owners")
		}
	})
}
