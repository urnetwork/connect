//go:build !js

package connect

import (
	"testing"
	"time"
)

// The existing H1 coalescer is an optimistic test-only upper bound for
// legacy SCTP. It must not be enabled for production P2P without separate
// wire-size, receive, lifecycle and retained-byte accounting proof.
func TestProviderUdpSctpReadyDrainKeepsColdAdmissionBoundary(t *testing.T) {
	assertMessagePoolOwnership(t)
	var results [2]udpSctpAckExperimentResult
	for index, name := range []string{"production-envelope", "stream-envelope-upper-bound"} {
		t.Run(name, func(t *testing.T) {
			results[index] = runProviderUdpSctpBatchExperiment(t, 120*time.Millisecond, false, index != 0)
			t.Logf("%+v", results[index])
		})
	}
	baseline, batched := results[0], results[1]
	if baseline.beforeAckAdmitted != batched.beforeAckAdmitted || baseline.beforeAckRefused != batched.beforeAckRefused || batched.beforeAckRefused == 0 {
		t.Fatalf("post-stall coalescing unexpectedly changed pre-first-ACK admission: baseline=%+v batched=%+v", baseline, batched)
	}
	if baseline.firstRefusal != batched.firstRefusal || baseline.firstAck != batched.firstAck {
		t.Fatalf("batching changed the cold SCTP flight: baseline=%+v batched=%+v", baseline, batched)
	}
	if batched.wireWrites >= int64(batched.admitted) {
		t.Fatalf("test-only stream envelope did not coalesce ordinary UDP: %+v", batched)
	}
}
