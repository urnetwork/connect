// A shared H1 path proof must not reopen delivery history for another carrier.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The old H1 service may outlive a policy change. Only current H1 sequences
// consume its path-proof boundary; sibling and legacy carrier rules survive.
func TestWindowPacingWindowProofPreservesOtherCarrierHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, metadata := range []bool{false, true} {
			sequences, service := newWindowRefillProofFixture(t, 1, metadata)
			sequence := sequences[0]
			at := completeWindowRefillProbe(t, sequence, service, metadata, 1210*time.Millisecond)
			sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{}}
			estimate := sequence.sendWindowEstimate(at)
			if estimate.Window >= estimate.Ceiling || estimate.Reason != "delivery" {
				t.Errorf("metadata=%t H1 path proof reopened another carrier's delivery history: %+v", metadata, estimate)
			}
		}
	})
}
