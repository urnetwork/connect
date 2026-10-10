package connect

import (
	"errors"
	"testing"
	"unsafe"
)

func TestProviderDatagramAckDetachedGraphFitsClaim(t *testing.T) {
	// The final detached record can be the last owner of the bounded source
	// evidence and transport attribution table, not just its own struct.
	known := unsafe.Sizeof(providerDatagramAck{}) + unsafe.Sizeof(preparedSendMemory{}) +
		unsafe.Sizeof(sourceAckEvidence{}) + unsafe.Sizeof(transportPacketAttribution{}) +
		unsafe.Sizeof(packetStatsCounters{}) + uintptr(len(TransportTypes()))*unsafe.Sizeof(PacketStats{})
	// One small map and the fixed producer/sequence wake channels. This is
	// an explicit envelope, not a claim to measure the whole Go allocator.
	const runtimeEnvelope = 512
	if ByteCount(known+runtimeEnvelope) > providerDatagramAckMemoryByteCount {
		t.Fatalf("detached graph envelope=%d exceeds charged bytes=%d", known+runtimeEnvelope, providerDatagramAckMemoryByteCount)
	}
}

func TestProviderDatagramAckFirstWriteDoesNotReleaseFlightOrCapsule(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(16))
	prepared, _ := prepareSendMemory(budget, kib(16))
	credit, _ := prepared.split(kib(16), providerDatagramAckMemoryByteCount)
	prepared.release()
	ack := &providerDatagramAck{credit: credit, wake: make(chan struct{}, 1)}
	item := &sendItem{}
	item.acks.add(sendAckRecord{target: ack})
	if !item.reservePreparedMemory(budget, kib(12)) {
		t.Fatal("prepaid transfer")
	}
	ack.admitted()
	ack.observeWrite(TransportTypeUnknown)
	if budget.UsedByteCount() != kib(12)+providerDatagramAckMemoryByteCount {
		t.Fatalf("first write released live ownership: %d", budget.UsedByteCount())
	}
	ack.abandon() // the NAT flow/provider can now be gone
	if budget.UsedByteCount() != kib(12)+providerDatagramAckMemoryByteCount {
		t.Fatal("actor cancellation released its accepted flight or detached capsule")
	}
	item.releaseMemory()
	ack.sendAckResult(0, nil)
	ack.sendAckResult(0, nil)
	assertRetainedBudgetBalance(t, budget)
}

func TestProviderDatagramAckInlineCompletionAndLateCancellation(t *testing.T) {
	for _, inline := range []bool{false, true} {
		t.Run(map[bool]string{false: "asynchronous", true: "inline"}[inline], func(t *testing.T) {
			budget := NewTransferMemoryBudget(kib(8))
			credit, _ := prepareSendMemory(budget, kib(8))
			evidence := &sourceAckEvidence{}
			observer := &preparedSendMemoryTestOwner{}
			ack := &providerDatagramAck{credit: credit, evidence: evidence, observer: observer, wake: make(chan struct{}, 1)}
			if inline {
				ack.sendAckResult(0, nil)
			}
			ack.admitted()
			if !inline {
				ack.abandon()
				if budget.UsedByteCount() != kib(8) {
					t.Fatal("accepted queue credit released before terminal ACK")
				}
				ack.sendAckResult(0, nil)
			}
			ack.sendAckResult(0, errors.New("duplicate"))
			ack.abandon()
			if observer.acks != 1 || evidence.outstanding.Load() != 0 || evidence.lastAckNanos.Load() == 0 {
				t.Fatalf("inline/late completion imbalance: callbacks=%d outstanding=%d", observer.acks, evidence.outstanding.Load())
			}
			assertRetainedBudgetBalance(t, budget)
		})
	}
}

func TestProviderDatagramAckOnlyProvenUnwrittenFailureRetainsRetryCredit(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(8))
	credit, _ := prepareSendMemory(budget, kib(8))
	ack := &providerDatagramAck{credit: credit, wake: make(chan struct{}, 1)}
	ack.admitted()
	ack.sendAckResult(0, errPreparedSendMemoryUnavailable)
	if budget.UsedByteCount() != kib(8) || providerDatagramUnwrittenRetry(ErrSendPackNotAdmitted) {
		t.Fatal("strictly-unwritten retry ownership or generic error classification changed")
	}
	ack.abandon()
	assertRetainedBudgetBalance(t, budget)
}
