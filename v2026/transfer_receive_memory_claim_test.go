// Attribute paid bytes to the exact live decoded owner. A different client's
// standing scratch reservation must not make a missing item claim look funded.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

type receiveMemoryLifecycleAttribution struct {
	sample        receiveMemoryLifecycleSample
	claim         ByteCount
	budgetMatches bool
	uniqueBinding bool
	quota         ByteCount
}

// Borrows the exclusively paused owner and returns only scalar observations.
func receiveMemoryLifecycleAttributionSample(gate *receiveMemoryLifecycleGate, sequence *ReceiveSequence, owner *decodedPackOwner, stage string) receiveMemoryLifecycleAttribution {
	result := receiveMemoryLifecycleAttribution{sample: gate.sample(sequence, owner, stage)}
	root := gate.root.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	result.quota = gate.pack.quotaByteCount.Load()
	if owner == nil || !owner.inUse {
		return result
	}
	result.claim = owner.receiveItem.queueByteCount
	result.budgetMatches = owner.receiveItem.memoryBudget == gate.rx && owner.receivePack.receiveMemoryOwned
	if sequence != nil && owner.receivePack.ingressSequence == sequence {
		matches := 0
		for pack := sequence.holdPromise.ingressHead; pack != nil; pack = pack.ingressNext {
			if pack == &owner.receivePack {
				matches++
			}
		}
		result.uniqueBinding = matches == 1
	}
	return result
}

// The separate old-compatible witness remains the physical-root regression.
// This witness keeps unrelated scratch above the whole item's cost,
// so only an exact per-item claim can satisfy the supplemental assertion.
func TestReceiveMemoryOwnerClaimExcludesStandingScratch(t *testing.T) {
	settings := receiveMemoryLifecycleTestSettings()
	assertMessagePoolOwnership(t)
	var observations []receiveMemoryLifecycleAttribution
	var scratchBytes ByteCount
	synctest.Test(t, func(t *testing.T) {
		rx, pack, clientBudget, root := receiveMemoryLifecycleBudgets(t, settings)
		scratchSettings := DefaultClientSettings()
		scratchSettings.ReceiveBufferSettings.ReceiveQueueBudget = rx
		scratchSettings.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting = true
		scratchSettings.EncryptionSettings.Mode = EncryptionModeRequired
		scratchClient := &Client{settings: scratchSettings}
		defer scratchClient.releaseReceiveDecodeMemory()
		if !scratchClient.prepareReceiveDecodeMemory(nil, 16*1024) {
			t.Fatal("standing scratch setup failed")
		}
		scratchBytes = scratchClient.receiveDecodeReservedByteCount.Load()
		if scratchBytes < 30544 {
			t.Fatal("scratch does not mask aggregate-only ownership oracle")
		}
		flow := newReceiveMemoryLifecycleFlow(t, settings, rx, pack)
		gate := newReceiveMemoryLifecycleGate(t, flow, root, clientBudget, rx, pack, "pack_released", "head_buffered")
		attributes := make(chan receiveMemoryLifecycleAttribution, 2)
		flow.fixture.receiver.receiveBuffer.receiveBufferSettings.receiveOwnerStageForTest = func(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if stage == "pack_released" || stage == "head_buffered" {
				attributes <- receiveMemoryLifecycleAttributionSample(gate, sequence, owner, stage)
			}
			gate.observe(sequence, owner, stage)
		}
		entered, resume := pauseReceiveMemoryLifecycleRun(t, flow)
		wire := flow.group(t, 8)
		gate.before(t, wire)
		flow.fixture.forward(wire, flow.fixture.receiverIn)
		<-entered
		synctest.Wait()
		sequence := flow.fixture.receiveSequence()
		if len(sequence.packs) != 1 {
			t.Fatal("queued attribution has no unique real owner")
		}
		queued := <-sequence.packs
		observations = append(observations, receiveMemoryLifecycleAttributionSample(gate, sequence, queued.decodedOwner, "pack_queued"))
		sequence.packs <- queued
		resume()
		synctest.Wait()
		for _, stage := range gate.stages {
			sample := <-stage.Entered
			attribute := <-attributes
			if attribute.sample.Stage != sample.Stage {
				t.Fatal("physical and claim boundaries differ")
			}
			if flow.fixture.deliveredCount != 0 || flow.fixture.ackedCount != 0 || len(flow.fixture.receiverOut) != 0 {
				t.Fatal("claim barrier passed callback/ACK")
			}
			observations = append(observations, attribute)
			close(stage.Resume)
			synctest.Wait()
		}
		time.Sleep(2 * settings.ackCompressTimeout)
		flow.fixture.acknowledge()
		if flow.fixture.deliveredCount != 8 || flow.fixture.ackedCount != 1 || flow.failed != 0 || flow.fixture.sequence().resendQueue.Len() != 0 {
			t.Fatal("claim witness failed real delivery/retirement")
		}
		joinReceiveMemoryLifecycleOwnedFlow(t, flow)
		if root.UsedByteCount() != scratchBytes || rx.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 {
			t.Fatal("item/quota teardown consumed unrelated scratch or leaked")
		}
		scratchClient.releaseReceiveDecodeMemory()
		if root.UsedByteCount() != 0 {
			t.Fatal("standing scratch did not return after all item owners")
		}
	})
	if len(observations) != 3 {
		t.Fatal("incomplete attributable owner lifetime")
	}
	for _, observation := range observations {
		sample := observation.sample
		assertReceiveMemoryLifecycleOwner(t, sample)
		// This condition intentionally also holds in the missing-claim fault.
		if sample.Root < sample.Cost || scratchBytes < sample.Cost {
			t.Fatal("standing-scratch masking premise was not established")
		}
		t.Logf("[receive-memory-owner-claim] stage=%s owned=%d item_claim=%d bound=%t unique=%t scratch=%d root=%d quota=%d", sample.Stage, sample.Cost, observation.claim, observation.budgetMatches, observation.uniqueBinding, scratchBytes, sample.Root, observation.quota)
		if !observation.budgetMatches || !observation.uniqueBinding || observation.claim < sample.Cost || sample.Root-scratchBytes < sample.Cost {
			t.Errorf("RX_RETAINED_OWNER_CLAIM_MISSING stage=%s owned=%d claim=%d bound=%t unique=%t scratch=%d root=%d", sample.Stage, sample.Cost, observation.claim, observation.budgetMatches, observation.uniqueBinding, scratchBytes, sample.Root)
		}
		if observation.budgetMatches && sample.Stage == "pack_queued" && observation.quota != sample.PackEncoded {
			t.Fatal("queued encoded permission differs from the exact handoff")
		}
		if sample.Stage != "pack_queued" && observation.quota != 0 {
			t.Fatal("dequeue retained encoded handoff permission")
		}
	}
}
