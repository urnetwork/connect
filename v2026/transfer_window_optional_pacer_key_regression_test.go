// The cleanup startup barrier belongs to one exact selected transfer key.
package connect

import "testing"

// A real unrelated worker must pass the barrier while the selected owner waits.
func TestWindowOptionalPacerCleanupBarrierIgnoresUnrelatedWorker(t *testing.T) {
	testWindowOptionalPacerCleanupStartup(t, true)
}
