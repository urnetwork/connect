package connect

import (
	"testing"
	"testing/synctest"
)

// If no tail can satisfy the larger request, the already returned decode
// reservation must remain usable. Releasing only the exact downstream deficit
// then lets the real source retry advance without another full decoder cell.
func TestReceiveMemoryProviderRestoredDecodeCreditUsesReleasedDeficit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiveProviderPressureFixtureWithHold(t, false, false)
		deficit := f.deliveryBytes - f.decodeBytes
		if deficit <= 0 || f.decodeBytes <= deficit {
			t.Fatal("restored-credit control requires a downstream deficit smaller than decoding")
		}
		f.releaseForeign(f.decodeBytes)
		f.forward(f.originals[1], f.receiverIn)
		state := f.sequence.memoryPressure
		if f.atNat() || f.sequence.receiveQueue.Len() != 0 || f.sequence.holdPromise.set ||
			!state.activeSet || state.owner != nil || state.reservedByteCount != f.decodeBytes || state.pendingSet ||
			f.root.Available() != 0 || f.ackedCount != 1 || len(f.receiverOut) != 0 {
			t.Fatal("no-tail control did not restore one actual decoded owner after its larger request failed")
		}
		f.releaseForeign(deficit)
		f.retryHead()
		stalled := !f.atNat()
		if stalled {
			state = f.sequence.memoryPressure
			if f.root.Available() != deficit || f.sequence.receiveQueue.Len() != 0 || f.sequence.holdPromise.set ||
				!state.activeSet || state.ready || state.owner != nil || state.reservedByteCount != f.decodeBytes || state.pendingSet ||
				f.ackedCount != 1 || f.ackCounts[1] != 0 || len(f.receiverOut) != 0 || f.sequence.ctx.Err() != nil {
				t.Fatal("restored-credit stall changed its exact capacity, original or resident premise")
			}
			t.Logf("[rx-restored-credit-unready] decode=%d released_deficit=%d escrow=%d ready=false eligible_tails=0", f.decodeBytes, deficit, state.reservedByteCount)
			// This extra full decode cell is a labeled validity rescue. The
			// exact downstream deficit above was already sufficient in total.
			f.releaseForeign(f.decodeBytes)
			f.retryHead()
		}
		f.proveNatCustody()
		f.drain()
		f.close()
		if stalled {
			t.Error("RX_PROVIDER_RESTORED_DECODE_CREDIT_NOT_READY: exact downstream capacity cannot reuse the already returned decoded reservation when no later tentative tail is available")
		}
	})
}
