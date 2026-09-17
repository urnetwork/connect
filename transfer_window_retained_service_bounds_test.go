// Service-qualified growth reports the bound that actually limits admission.
package connect

import (
	"testing"
	"testing/synctest"
)

// The service supports more flight than each hard limit. Its diagnostic must
// identify that limit while retaining the complete qualified service evidence.
func TestWindowRetainedServiceGrowthReportsHardBound(t *testing.T) {
	for _, test := range []struct {
		bound  string
		bytes  ByteCount
		reason string
	}{
		{bound: "peer", bytes: 4 * 1024 * 1024, reason: "the peer's advertised capacity"},
		{bound: "memory", bytes: 4 * 1024 * 1024, reason: "the memory budget's share"},
		{bound: "configured", bytes: 4 * 1024 * 1024, reason: "the configured byte ceiling"},
		{bound: "floor", bytes: 32 * 1024, reason: "floor"},
	} {
		synctest.Test(t, func(t *testing.T) {
			sequence, at := newRetainedServiceGrowthFixture(t, 512)
			switch test.bound {
			case "peer", "floor":
				sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: uint32(test.bytes)})
			case "memory":
				sequence.resendQueue.Budget().SetTotalByteCount(test.bytes)
			case "configured":
				sequence.sendBufferSettings.DeliverySizedWindowCeilingByteCount = test.bytes
			}
			estimate := sequence.sendWindowEstimate(at)
			if estimate.Sized || !estimate.ServiceSized || estimate.Window != test.bytes || estimate.CandidateWindow != test.bytes || estimate.Ceiling != test.bytes || estimate.LearnedWindow != max(estimate.Initial, test.bytes) {
				t.Fatalf("%s fixture did not isolate a hard service bound: %+v", test.bound, estimate)
			}
			if estimate.Reason != test.reason || estimate.TargetBound || estimate.CandidateTargetBound {
				t.Errorf("%s hard bound was mislabeled: %+v, want %q", test.bound, estimate, test.reason)
			}
		})
	}
}

// A lower target can remain the true service candidate bound when hard byte
// permission is larger. Hard-bound diagnostics must preserve that distinction.
func TestWindowRetainedServiceGrowthReportsTargetBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, at := newRetainedServiceGrowthFixture(t, 512)
		sequence.sendBufferSettings.TargetGoodputByteRate = 20000000
		estimate := sequence.sendWindowEstimate(at)
		if estimate.Sized || !estimate.ServiceSized || !estimate.TargetBound || !estimate.CandidateTargetBound || estimate.Reason != "target" || estimate.Window <= estimate.Initial || estimate.Window >= estimate.Ceiling {
			t.Fatalf("qualified target-bound service lost its diagnostic: %+v", estimate)
		}
	})
}

// Service remains the reason when the required flight is below every hard
// bound. An unrelated permission must not be blamed for that smaller window.
func TestWindowRetainedServiceGrowthReportsServiceBelowBounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, at := newRetainedServiceGrowthFixture(t, 512)
		sequence.sendBufferSettings.TargetGoodputByteRate = 0
		sequence.resendQueue.Budget().SetTotalByteCount(128 * 1024 * 1024)
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: 128 * 1024 * 1024})
		estimate := sequence.sendWindowEstimate(at)
		if estimate.Sized || !estimate.ServiceSized || estimate.TargetBound || estimate.CandidateTargetBound || estimate.Reason != "measured service" || estimate.Window <= estimate.Initial || estimate.Window >= estimate.Ceiling {
			t.Fatalf("unclamped service growth lost its diagnostic: %+v", estimate)
		}
	})
}
