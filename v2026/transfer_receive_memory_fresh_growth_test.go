package connect

import (
	"testing"
	"testing/synctest"
)

// A decoder that fits on every attempt must not strand recoverable downstream
// custody merely because predecode pressure never created an active escrow.
func TestReceiveMemoryProviderFreshDecodeCanRecoverDownstreamCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiveProviderPressureFixture(t)
		f.releaseForeign(f.decodeBytes)
		if f.root.Available() != f.decodeBytes || f.sequence.memoryPressure != (receiveMemoryPressure{}) {
			t.Fatal("fresh-decode fixture did not isolate decode-sized free capacity without pressure")
		}
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
			if f.ackedCount != 1 || f.root.Available() != f.decodeBytes ||
				f.sequence.receiveQueue.GetBySequenceNumber(10) == nil ||
				f.sequence.receiveQueue.GetBySequenceNumber(11) == nil ||
				f.sequence.ctx.Err() != nil {
				t.Fatal("fresh-decode stall changed its actual ownership or source premise")
			}
			t.Logf("[rx-fresh-decode-stall] decode=%d downstream=%d reclaimable=%d retries=3", f.decodeBytes, f.deliveryBytes-f.decodeBytes, 2*f.deliveryBytes)
			// Explicit external-capacity rescue proves the same source and
			// receipt/NAT pipeline can complete; it does not erase the stall.
			f.releaseForeign(f.deliveryBytes - f.decodeBytes)
			f.retryHead()
		}
		f.proveNatCustody()
		f.drain()
		f.close()
		if stalled {
			t.Error("RX_PROVIDER_FRESH_DECODE_GROWTH_STALLED: decode fits repeatedly but downstream recovery ignores later uncommitted owners")
		}
	})
}
