package connect

import "testing"

// The callbacks execute a possible writer interleaving using the same atomic
// stores as admit. No timing loop or scheduler luck is needed for this window.
func TestTransferMemoryOwnerSnapshotWriterFinishesInsideEndFence(t *testing.T) {
	var ledger TransferMemoryOwnerLedger
	const slots = 32
	view := ledger.snapshot(func(final bool) {
		if !final {
			ledger.writers.Add(1)
			ledger.send.workers.Add(1)
			return
		}
		// The reader has observed workers=1, slots=0. Finish the writer
		// between the two endpoint loads. That partial view must be refused.
		ledger.send.channelBytes.Add(slots)
		ledger.send.admitted.Add(1)
		ledger.revision.Add(1)
		ledger.writers.Add(-1)
	})
	if view.Send.Workers != 1 || view.Send.KnownChannelSlotBytes != 0 {
		t.Fatalf("control did not hold the partial byte update: %+v", view.Send)
	}
	if view.Complete {
		t.Fatal("writer completed inside end fence but partial ownership was qualified")
	}
	healthy := ledger.Snapshot()
	if !healthy.Complete || healthy.Send.Workers != 1 || healthy.Send.KnownChannelSlotBytes != slots {
		t.Fatalf("completed writer must restore a coherent sample: %+v", healthy)
	}
}

func TestTransferMemoryOwnerSnapshotWriterBeginsInsideEndFence(t *testing.T) {
	var ledger TransferMemoryOwnerLedger
	view := ledger.snapshot(func(final bool) {
		if final {
			ledger.admit(transferMemoryOwnerSend, 32)
		}
	})
	if view.Complete {
		t.Fatal("a complete lifecycle update inside the end fence was missed")
	}
	if healthy := ledger.Snapshot(); !healthy.Complete || healthy.Send.KnownChannelSlotBytes != 32 {
		t.Fatalf("subsequent healthy sample was not restored: %+v", healthy)
	}
}
