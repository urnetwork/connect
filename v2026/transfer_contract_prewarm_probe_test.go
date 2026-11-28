// The opening-contract prewarm and the process-wide degraded-backend gate. The
// gate holds the acquisition loop and successor requests; the prewarm is the
// one contract request it does not hold, and so the request whose success
// clears a tripped gate.
package connect

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The prewarm goes out through a tripped gate and its completed round trip
// clears it, so the first message takes the contract the prewarm brought.
// Held by the gate, a new sequence sent nothing until the gate's 2 minute
// recency window ran out: replayed in order, five transfer tests that follow
// one leaving the gate tripped waited 15 to 30 s for their contracts and
// failed.
func TestPrewarmOpeningContractClearsATrippedBackendGate(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		for range backendDegradedFailThreshold {
			noteBackendFailure()
		}
		if !isBackendDegraded() {
			t.Fatal("precondition: the gate did not trip")
		}
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryGrant },
		}
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = true
		client := newContractRetryTestClient(t, oob, settings)

		destinationId := NewId()
		sendContractRetryMessage(t, client, destinationId)
		synctest.Wait()
		requireContractRetryOffsets(t, "prewarm through the tripped gate", oob.requestOffsets(destinationId, start), []time.Duration{0})
		if isBackendDegraded() {
			t.Fatal("the prewarm's completed round trip did not clear the gate")
		}
		if stats := client.ContractManager().LocalStats(); stats.ContractOpenCount != 1 {
			t.Fatalf("open contracts = %d, want the prewarmed contract taken", stats.ContractOpenCount)
		}
	})
}

// A healthy backend keeps the prewarm: by the sequence's first queue take it
// is the only request, and the first message takes the contract it brought.
func TestPrewarmOpeningContractRequestsWhileBackendHealthy(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		destinationId := NewId()
		takeEntered := make(chan struct{})
		takeRelease := make(chan struct{})
		var takeOnce sync.Once
		var releaseOnce sync.Once
		releaseTake := func() { releaseOnce.Do(func() { close(takeRelease) }) }
		defer releaseTake()

		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryGrant },
		}
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = true
		settings.SendBufferSettings.backendDegradedForTest = func() bool { return false }
		settings.SendBufferSettings.beforeTakeContractForTest = func(id sendSequenceId) {
			if id.Destination != destinationId {
				return
			}
			takeOnce.Do(func() {
				close(takeEntered)
				<-takeRelease
			})
		}
		client := newContractRetryTestClient(t, oob, settings)
		t.Cleanup(releaseTake)

		sendContractRetryMessage(t, client, destinationId)
		select {
		case <-takeEntered:
		case <-time.After(time.Minute):
			t.Fatal("the sequence never reached its opening contract take")
		}
		requireContractRetryOffsets(t, "prewarm", oob.requestOffsets(destinationId, start), []time.Duration{0})

		releaseTake()
		synctest.Wait()
		requireContractRetryOffsets(t, "opening contract", oob.requestOffsets(destinationId, start), []time.Duration{0})
		if stats := client.ContractManager().LocalStats(); stats.ContractOpenCount != 1 {
			t.Fatalf("open contracts = %d, want the prewarmed contract taken", stats.ContractOpenCount)
		}
	})
}
