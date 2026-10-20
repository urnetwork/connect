package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"unsafe"
)

func TestTransferProductionAckOwnerDoesNotReserveCompatibilityQueue(t *testing.T) {
	settings := closeWaitClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 4096
	settings.SendBufferSettings.AckBufferSize = 4096
	client := NewClient(t.Context(), NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(func() {
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	const owners = 32
	var unusedSlots, dataSlots uintptr
	for range owners {
		sequence := NewSendSequence(t.Context(), client, client.sendBuffer, NewId(),
			MultiHopId{}, false, false, false, sequenceTlsRoleClient, false, settings.SendBufferSettings)
		t.Cleanup(sequence.Close)
		if sequence.ackWindow == nil || sequence.resendQueue == nil {
			t.Fatal("constructor did not publish direct ACK ownership")
		}
		unusedSlots += uintptr(cap(sequence.acks)) * unsafe.Sizeof(receiveAckMessage{})
		dataSlots += uintptr(cap(sequence.packs)) * unsafe.Sizeof((*SendPack)(nil))
	}
	t.Logf("owners=%d compatibility_ack_bytes=%d data_channel_bytes=%d", owners, unusedSlots, dataSlots)
	if dataSlots != owners*4096*unsafe.Sizeof((*SendPack)(nil)) {
		t.Fatal("data admission capacity changed")
	}
	if unusedSlots != 0 {
		t.Fatalf("direct ACK owners still reserve %d unused compatibility channel bytes", unusedSlots)
	}
}

func TestTransferProductionAckOwnerProgressWithoutCompatibilityWorker(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var compatibilityVisits atomic.Int64
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.AckBufferSize = 4096
			settings.SendBufferSettings.beforeAckWorkerReceiveForTest = func(sendSequenceId) {
				compatibilityVisits.Add(1)
			}
		})
		packet := fixture.write(64)
		sequence := fixture.sequence()
		ack := fixture.receive(packet)
		fixture.forward(ack, fixture.senderIn)
		synctest.Wait()
		if fixture.deliveredCount != 1 || fixture.ackedCount != 1 || sequence.resendQueue.Len() != 0 {
			t.Fatal("direct ACK path did not deliver and release exactly once")
		}
		if n := compatibilityVisits.Load(); n != 0 {
			t.Fatalf("production started an unused compatibility ACK worker: visits=%d", n)
		}
	})
}

func TestTransferConstructedLegacyAckOwnerKeepsBoundedChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sequence := &SendSequence{ctx: ctx, cancel: cancel, client: &Client{},
		sequenceId: NewId(), acks: make(chan receiveAckMessage, 1)}
	ack := receiveAckMessage{sequenceId: sequence.sequenceId, messageId: NewId()}
	result, err := sequence.ackMessageDetailed(ack, 0)
	if result != receiveAckHandoffAccepted || err != nil {
		t.Fatalf("legacy first handoff failed: %d %v", result, err)
	}
	result, err = sequence.ackMessageDetailed(ack, 0)
	if result != receiveAckHandoffQueueFull || err != nil {
		t.Fatalf("legacy bounded channel lost backpressure: %d %v", result, err)
	}
	if got := <-sequence.acks; got.messageId != ack.messageId {
		t.Fatal("legacy queue did not retain the accepted identity")
	}
	cancel()
	if result, err = sequence.ackMessageDetailed(ack, 0); result != receiveAckHandoffSequenceClosed || err == nil {
		t.Fatalf("canceled legacy owner accepted feedback: %d %v", result, err)
	}
}

func TestTransferConstructedLegacyAckWorkerProgress(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var sender *Client
		var destination Id
		var visits atomic.Int64
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
				if id.Destination != destination {
					return
				}
				// Before Run or the first wire write, construct the historical
				// queue-only owner. Production constructors never do this.
				sender.sendBuffer.mutex.Lock()
				sequence := sender.sendBuffer.sendSequences[id]
				sequence.acks = make(chan receiveAckMessage, 1)
				sequence.ackWindow = nil
				sender.sendBuffer.mutex.Unlock()
			}
			settings.SendBufferSettings.beforeAckWorkerReceiveForTest = func(sendSequenceId) {
				visits.Add(1)
			}
		})
		sender, destination = fixture.sender, fixture.receiver.ClientId()
		packet := fixture.write(64)
		sequence := fixture.sequence()
		ack := fixture.receive(packet)
		fixture.forward(ack, fixture.senderIn)
		synctest.Wait()
		if visits.Load() == 0 || cap(sequence.acks) != 1 || len(sequence.acks) != 0 ||
			fixture.deliveredCount != 1 || fixture.ackedCount != 1 || sequence.resendQueue.Len() != 0 {
			t.Fatal("constructed legacy worker lost bounded delivery/ACK progress")
		}
	})
}

func BenchmarkTransferProductionAckOwner(b *testing.B) {
	settings := closeWaitClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 4096
	settings.SendBufferSettings.AckBufferSize = 4096
	client := NewClient(b.Context(), NewId(), NewNoContractClientOob(), settings)
	b.Cleanup(func() {
		if err := client.CloseAndWait(context.Background()); err != nil {
			b.Error(err)
		}
	})
	b.ReportAllocs()
	b.ResetTimer()
	var compatibilityBytes uintptr
	for range b.N {
		sequence := NewSendSequence(b.Context(), client, client.sendBuffer, NewId(),
			MultiHopId{}, false, false, false, sequenceTlsRoleClient, false, settings.SendBufferSettings)
		compatibilityBytes = uintptr(cap(sequence.acks)) * unsafe.Sizeof(receiveAckMessage{})
		sequence.Close()
	}
	b.StopTimer()
	b.ReportMetric(float64(compatibilityBytes), "compat-ack-B/op")
}
