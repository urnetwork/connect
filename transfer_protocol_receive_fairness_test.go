package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Refill the actual protocol mailbox before its owner resumes, while DATA
// is already queued. Priority must offer the ready DATA a turn rather than
// draining a continuously replenished protocol source indefinitely.
func TestProtocolReceivePriorityYieldsToReadyData(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		workerEntered, workerResume := make(chan struct{}), make(chan struct{})
		controlEntered, controlResume := make(chan struct{}), make(chan struct{})
		var workerOnce, resumeWorkerOnce, resumeControlOnce sync.Once
		var releasedTurns atomic.Int32
		var secondWasData atomic.Bool
		resumeWorker := func() { resumeWorkerOnce.Do(func() { close(workerResume) }) }
		resumeControl := func() { resumeControlOnce.Do(func() { close(controlResume) }) }
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
				workerOnce.Do(func() { close(workerEntered) })
				<-workerResume
			}
			settings.ReceiveBufferSettings.receiveOwnerStageForTest = func(sequence *ReceiveSequence, _ *decodedPackOwner, stage string) {
				if stage != "pack_released" {
					return
				}
				switch releasedTurns.Add(1) {
				case 1:
					close(controlEntered)
					<-controlResume
				case 2:
					secondWasData.Store(len(sequence.packs) == 0 && len(sequence.protocolPacks) == 1)
				}
			}
		})
		t.Cleanup(resumeWorker)
		t.Cleanup(resumeControl)
		var dataDelivered atomic.Bool
		var controlTurns atomic.Int32
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_TestSimpleMessage {
					dataDelivered.Store(true)
					continue
				}
				if protocolOnlyFrame(frame.MessageType, frame.Raw, frame.MessageBytes) {
					controlTurns.Add(1)
				}
			}
		})
		data := fixture.write(64)
		fixture.forward(data, fixture.receiverIn)
		<-workerEntered
		sendControl := func() {
			packet := MessagePoolCopy(ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 7000, nil))
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}
			if admitted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), nil, time.Second, NoBudget(), NoAck()); !admitted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("public protocol offer failed: %t %v", admitted, err)
			}
			wire := fixture.take(fixture.senderOut)
			if wire.pack == nil || !wire.pack.Nack || !protocolOnlyPack(wire.pack) {
				t.Fatal("public independently delivered protocol changed its wire class")
			}
			fixture.forward(wire, fixture.receiverIn)
		}
		sendControl()
		sequence := fixture.receiveSequence()
		if len(sequence.packs) != 1 || len(sequence.protocolPacks) != 1 {
			t.Fatal("worker barrier did not retain both real input classes")
		}
		resumeWorker()
		<-controlEntered
		sendControl()
		if len(sequence.packs) != 1 || len(sequence.protocolPacks) != 1 || dataDelivered.Load() {
			t.Fatalf("owner barrier did not retain competing mailboxes: DATA=%d protocol=%d delivered=%t", len(sequence.packs), len(sequence.protocolPacks), dataDelivered.Load())
		}
		resumeControl()
		synctest.Wait()
		fixture.acknowledge()
		synctest.Wait()
		if !dataDelivered.Load() || controlTurns.Load() != 2 || !secondWasData.Load() || releasedTurns.Load() != 3 || fixture.ackedCount != 1 {
			t.Fatalf("ready DATA lost its turn: data=%t controls=%d second DATA=%t dequeued=%d data ACKs=%d", dataDelivered.Load(), controlTurns.Load(), secondWasData.Load(), releasedTurns.Load(), fixture.ackedCount)
		}
	})
}
