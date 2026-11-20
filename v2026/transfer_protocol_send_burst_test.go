// Protocol byte exemption keeps the configured public send burst capacity.
// A cold worker cannot acknowledge or consume either caller-owned original.
package connect

import (
	"bytes"
	"sync"
	"testing"
	"testing/synctest"
)

// Four zero-time reliable control offers fit the existing four-slot policy
// before the actual worker starts. Real delivery alone releases their owners.
func TestProtocolSendColdBurstPreservesConfiguredCount(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const perLaneCount = 4
		const configuredCount = perLaneCount * maxLogicalDataLaneCount
		budget := NewTransferMemoryBudget(0)
		t.Cleanup(func() { assertRetainedBudgetBalance(t, budget) })
		entered, resume := make(chan struct{}), make(chan struct{})
		var resumeOnce sync.Once
		release := func() { resumeOnce.Do(func() { close(resume) }) }
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.SequenceBufferSize = configuredCount
			settings.ResendQueueBudget = budget
			settings.ResendQueueRetainedByteAccounting = true
			settings.ResendQueueMinByteCount = 0
			settings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
				if id.LogicalLane == 1 {
					close(entered)
					<-resume
				}
			}
		}, nil, TransportTypeH1)
		// Release before the fixture joins its worker, including the expected
		// old-policy failure at the second offer.
		t.Cleanup(release)
		var results [perLaneCount]protocolPressureCompletion
		var expected [perLaneCount][]byte
		for index := range results {
			expected[index] = ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, uint32(7000+index), nil)
			packet := MessagePoolCopy(expected[index])
			frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
			if err != nil || !protocolOnlyFrame(frame.MessageType, frame.Raw, frame.MessageBytes) {
				MessagePoolReturn(packet)
				t.Fatalf("pure TCP control construction: %v", err)
			}
			admitted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), results[index].record, 0, TransferKey{LogicalLane: 1}, NoBudget())
			if !admitted {
				MessagePoolReturn(frame.MessageBytes)
			}
			if !admitted || err != nil {
				t.Fatalf("public protocol burst narrowed configured send count: offer=%d configured=%d per_lane=%d admitted=%t error=%v", index+1, configuredCount, perLaneCount, admitted, err)
			}
			if index == 0 {
				<-entered
				sequence := fixture.sequence()
				if sequence.packAdmission == nil || sequence.protocolAdmission == nil ||
					sequence.packAdmission.capacity != perLaneCount || sequence.protocolAdmission.capacity != perLaneCount ||
					cap(sequence.packs) != 2*perLaneCount || sequence.protocolPacks != nil {
					t.Fatal("cold public send changed independent class counts or combined FIFO capacity")
				}
				t.Logf("cold public send geometry: lane=1 configured=%d data_count=%d protocol_count=%d fifo_capacity=%d", configuredCount, sequence.packAdmission.capacity, sequence.protocolAdmission.capacity, cap(sequence.packs))
			}
		}
		// The next public zero-time offer must retain caller custody even though
		// the combined FIFO still has the independent DATA class's slots free.
		extra := MessagePoolCopy(expected[0])
		extraFrame, err := ipPacketToProviderFrame(extra, DefaultProtocolVersion)
		if err != nil {
			MessagePoolReturn(extra)
			t.Fatal(err)
		}
		var refused protocolPressureCompletion
		admitted, err := fixture.sender.SendWithTimeoutDetailed(extraFrame, fixture.receiver.ClientId(), refused.record, 0, TransferKey{LogicalLane: 1}, NoBudget())
		if !admitted || err != nil {
			MessagePoolReturn(extraFrame.MessageBytes)
		}
		if admitted || err != nil || refused.calls.Load() != 0 {
			t.Fatalf("protocol count limit lost refusal custody: admitted=%t error=%v callbacks=%d", admitted, err, refused.calls.Load())
		}
		synctest.Wait()
		if len(fixture.senderOut) != 0 || fixture.deliveredCount != 0 {
			t.Fatal("cold barrier allowed an early write")
		}
		for index := range results {
			if results[index].calls.Load() != 0 {
				t.Fatal("raw control admission produced a false delivery acknowledgement")
			}
		}
		assertRetainedBudgetBalance(t, budget)
		release()
		var seen [perLaneCount]bool
		frames := 0
		for frames < len(results) {
			wire := fixture.take(fixture.senderOut)
			if wire.pack == nil || wire.pack.Nack || wire.pack.LogicalLane != 1 || len(wire.pack.Frames) == 0 {
				t.Fatal("public reliable protocol burst changed its actual wire policy")
			}
			for _, frame := range wire.pack.Frames {
				matched := false
				for index, packet := range expected {
					if !seen[index] && frame.Raw && bytes.Equal(frame.MessageBytes, packet) {
						seen[index], matched = true, true
						frames++
						break
					}
				}
				if !matched {
					t.Fatal("actual burst lost or repeated a caller's control packet")
				}
			}
			fixture.forward(wire, fixture.receiverIn)
		}
		if fixture.deliveredCount != len(results) {
			t.Fatal("all admitted protocol originals did not reach the real receive callback")
		}
		for index := range results {
			if results[index].calls.Load() != 0 {
				t.Fatal("protocol completion preceded the actual peer acknowledgement")
			}
		}
		fixture.acknowledge()
		for index := range results {
			if results[index].calls.Load() != 1 || results[index].success.Load() != 1 {
				t.Fatalf("control %d did not complete exactly once after peer acknowledgement", index)
			}
		}
		assertRetainedBudgetBalance(t, budget)
	})
}
