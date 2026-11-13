// Candidate-only workspace checks follow the same actual socket-worker drain.
package connect

import (
	"testing"
	"testing/synctest"
)

// Keep these new fields out of the behavior leaf used for the pre-fix run.
func TestTcpAdaptiveReadQueueWorkerReleasesWorkspace(t *testing.T) {
	runTcpAdaptiveReadQueueWorker(t, func(t *testing.T, sequence *TcpSequence) {
		// Consuming the final published Pack does not join its socket callback.
		// All18 bytes and the writer result are already drained. Let the live
		// bubble finish the callback and finishBatch before checking its claim;
		// fixture Cleanup has not run and shared parents remain live.
		synctest.Wait()
		workspace := sequence.ipPath.tcpReturnBatchWorkspace
		if workspace == nil || workspace.inUse.Load() || workspace.funded.Load() {
			t.Fatal("completed socket callback retained funded workspace")
		}
	})
}
