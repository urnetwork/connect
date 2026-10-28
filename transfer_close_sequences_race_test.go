// Forces close requests across sequence exit, contract handoff and reliable
// delivery waits so settlement cannot discard counted work or stall on data.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// Connects a real control receiver while keeping its acknowledgement route
// absent until released. This separates frame construction from delivery ACK.
func (self *closeSequencesHarness) holdControlAcknowledgements() func() {
	self.t.Helper()
	settings := closeWaitClientSettings()
	settings.ReceiveBufferSettings.AckCompressTimeout = 0
	receiver := NewClient(self.t.Context(), ControlId, NewNoContractClientOob(), settings)
	self.client.ContractManager().AddNoContractPeer(ControlId)
	receiver.ContractManager().AddNoContractPeer(self.client.ClientId())
	sendRoute, ackRoute := make(Route), make(Route)
	sendTransport := NewSendGatewayTransport()
	receiveTransport := NewReceiveGatewayTransport()
	self.client.RouteManager().UpdateTransport(sendTransport, []Route{sendRoute})
	receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{sendRoute})
	self.client.RouteManager().UpdateTransport(receiveTransport, []Route{ackRoute})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			receiver.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{ackRoute})
		})
	}
	self.t.Cleanup(func() {
		for _, client := range []*Client{self.client, receiver} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				self.t.Error(err)
			}
		}
		self.client.RouteManager().RemoveTransport(sendTransport)
		self.client.RouteManager().RemoveTransport(receiveTransport)
	})
	return release
}

// Holds a counted checkpoint across the owner handoff and withholds its native
// ACK. Either kind of platform close must wait for acknowledged delivery.
func runCloseSequencesReceiveExit(t *testing.T, closePeer bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		h := newCloseSequencesHarness(t)
		releaseAcknowledgements := h.holdControlAcknowledgements()
		sender := h.newSender()
		contractId, frame := h.newReceiveContract(sender.sourceId)
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseReport := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseReport)
		capture := h.client.ContractManager().beforeOriginalCloseFrameForTest
		h.client.ContractManager().beforeOriginalCloseFrameForTest = func(report *protocol.CloseContract) {
			if RequireIdFromBytes(report.ContractId) == contractId && report.AckedByteCount > 0 {
				close(entered)
				<-release
			}
			capture(report)
		}
		bytes := h.deliver(sender, frame, closeSequencesMessageFrame("delivered-before-close"))
		synctest.Wait()
		sequence := h.receiveSequences(sender.sourceId)[0]
		sequence.Cancel()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("receive sequence did not reach counted checkpoint")
		}
		if closePeer {
			h.closeSequences(nil, []Id{sender.sourceId})
		} else {
			h.closeSequences([]Id{contractId}, nil)
		}
		synctest.Wait()
		releaseReport()
		synctest.Wait()
		h.requireReports(contractId, closeSequencesReport{acked: uint64(bytes), checkpoint: true})
		// Repeated requests after the holder leaves still coalesce behind the
		// existing report, including while its native acknowledgement waits.
		for range 3 {
			h.closeSequences([]Id{contractId}, nil)
			synctest.Wait()
		}
		h.requireReports(contractId, closeSequencesReport{acked: uint64(bytes), checkpoint: true})
		releaseAcknowledgements()
		synctest.Wait()
		h.requireReports(contractId,
			closeSequencesReport{acked: uint64(bytes), checkpoint: true},
			closeSequencesReport{},
		)
	})
}

// A counted report must precede fallback zero final for the same contract.
func TestCloseSequencesDuringReceiveExitPreservesReportOrder(t *testing.T) {
	runCloseSequencesReceiveExit(t, false)
}

// Peer retirement racing an already chosen checkpoint must still finalize it.
func TestCloseSequencesDuringReceiveExitPeerClosePreservesReportOrder(t *testing.T) {
	runCloseSequencesReceiveExit(t, true)
}

// A counted send final keeps ownership while its frame is still being built.
func TestCloseSequencesDuringSendExitPreservesReportOrder(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		h := newCloseSequencesHarness(t)
		destinationId := NewId()
		route := make(Route, 64)
		transport := NewSendGatewayTransport()
		h.client.RouteManager().UpdateTransport(transport, []Route{route})
		t.Cleanup(func() {
			if err := h.client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			h.client.RouteManager().RemoveTransport(transport)
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		})
		contractId := h.queueSendContract(ContractKey{Destination: DestinationId(destinationId)})
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseReport := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseReport)
		capture := h.client.ContractManager().beforeOriginalCloseFrameForTest
		h.client.ContractManager().beforeOriginalCloseFrameForTest = func(report *protocol.CloseContract) {
			if RequireIdFromBytes(report.ContractId) == contractId && report.AckedByteCount > 0 {
				close(entered)
				<-release
			}
			capture(report)
		}
		h.send(destinationId, "sent-before-close")
		synctest.Wait()
		sequence := h.client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destinationId}, nil)
		// Acknowledge the opening contract and data through the sequence's
		// actual handoff so the final contains billed acknowledged bytes.
		for range 3 {
			item := sequence.resendQueue.PeekLast()
			if item == nil {
				break
			}
			if ok, err := sequence.Ack(&protocol.Ack{MessageId: item.messageId.Bytes(), SequenceId: sequence.sequenceId.Bytes()}, 0); !ok || err != nil {
				t.Fatalf("ACK admission = %t, %v", ok, err)
			}
			synctest.Wait()
		}
		if sequence.openSendContracts[contractId].ackedByteCount == 0 {
			t.Fatal("fixture did not acknowledge application bytes")
		}
		sequence.Cancel()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("send sequence did not reach counted final")
		}
		h.closeSequences([]Id{contractId}, nil)
		synctest.Wait()
		releaseReport()
		synctest.Wait()
		reports := h.reportsFor(contractId)
		if len(reports) == 0 || reports[0].acked == 0 || reports[0].checkpoint {
			t.Fatalf("reports = %+v; want one counted final before any fallback zero", reports)
		}
	})
}

// Removing a queued send contract must publish its open owner in the same
// manager critical section, so a close lookup cannot mistake it for unheld.
func TestCloseSequencesDuringQueuedContractHandoffKeepsOwner(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		h := newCloseSequencesHarness(t)
		destinationId := NewId()
		contractId := h.queueSendContract(ContractKey{Destination: DestinationId(destinationId)})
		manager := h.client.ContractManager()
		entered, release := make(chan struct{}), make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(resume)
		manager.afterPollContractForTest = func() {
			enteredOnce.Do(func() {
				close(entered)
				<-release
			})
		}
		h.send(destinationId, "pending-handoff")
		<-entered
		if manager.mutex.TryLock() {
			manager.mutex.Unlock()
			t.Fatal("queue removal released the close lookup lock before publishing its open owner")
		}
		go h.closeSequences([]Id{contractId}, nil)
		resume()
		synctest.Wait()
		reports := h.reportsFor(contractId)
		if len(reports) != 1 || reports[0].checkpoint {
			t.Fatalf("handoff close reports = %+v, want one final from the owning sequence", reports)
		}
		if h.client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destinationId}, nil) != nil {
			t.Fatal("close lost the sequence that had taken its queued contract")
		}
	})
}

// Downstream capacity cannot hide a current-contract close notification.
func TestCloseSequencesWakesPendingDelivery(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		h := newCloseSequencesHarness(t)
		sender := h.newSender()
		contractId, contractFrame := h.newReceiveContract(sender.sourceId)
		path := reliableDeliveryTestPath(4)
		flow, _ := ipPacketFlowKeyFromPath(path)
		capacity := make(chan struct{})
		h.client.AddReceiveCallback(func(source TransferPath, _ []*protocol.Frame, peer Peer) {
			if source.SourceId != sender.sourceId {
				return
			}
			receipt := peer.deliveryReceipt()
			if receipt == nil {
				t.Error("TCP delivery did not acquire its receipt")
				return
			}
			receipt.operation(flow, false,
				func() <-chan struct{} { return capacity },
				func() receiveDeliveryAttempt { return receiveDeliveryWaiting },
				nil,
			)
		})
		packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, 101, []byte("pending delivery")))
		frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil {
			MessagePoolReturn(packet)
			t.Fatal(err)
		}
		h.deliver(sender, contractFrame, frame)
		synctest.Wait()
		sequence := h.receiveSequences(sender.sourceId)[0]
		if sequence.deliveryQueue == nil || !sequence.deliveryQueue.pending() {
			t.Fatal("fixture did not enter pending delivery wait")
		}
		h.closeSequences([]Id{contractId}, nil)
		synctest.Wait()
		if reports := h.reportsFor(contractId); len(reports) == 0 {
			t.Fatal("current contract close did not wake pending delivery; it still waits for downstream capacity or idle timeout")
		}
	})
}

// A close for a preceding contract must wake the same capacity wait without
// stopping the live current contract or waiting for downstream flow capacity.
func TestCloseSequencesWakesPendingDeliveryForPreviousContract(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		h := newCloseSequencesHarness(t)
		sender := h.newSender()
		previousId, previousFrame := h.newReceiveContract(sender.sourceId)
		previousBytes := h.deliver(sender, previousFrame, closeSequencesMessageFrame("previous"))
		synctest.Wait()
		currentId, currentFrame := h.newReceiveContract(sender.sourceId)
		path := reliableDeliveryTestPath(5)
		flow, _ := ipPacketFlowKeyFromPath(path)
		capacity := make(chan struct{})
		h.client.AddReceiveCallback(func(source TransferPath, _ []*protocol.Frame, peer Peer) {
			if source.SourceId != sender.sourceId {
				return
			}
			if receipt := peer.deliveryReceipt(); receipt != nil {
				receipt.operation(flow, false,
					func() <-chan struct{} { return capacity },
					func() receiveDeliveryAttempt { return receiveDeliveryWaiting }, nil,
				)
			}
		})
		packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, 102, []byte("current pending delivery")))
		frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil {
			MessagePoolReturn(packet)
			t.Fatal(err)
		}
		h.deliver(sender, currentFrame, frame)
		synctest.Wait()
		sequence := h.receiveSequences(sender.sourceId)[0]
		if sequence.receiveContract.contractId != currentId || sequence.deliveryQueue == nil || !sequence.deliveryQueue.pending() {
			t.Fatal("fixture did not keep the current contract pending on downstream delivery")
		}
		h.closeSequences([]Id{previousId}, nil)
		synctest.Wait()
		h.requireReports(previousId, closeSequencesReport{acked: uint64(previousBytes)})
		h.requireReports(currentId)
		if sequence.ctx.Err() != nil || !sequence.deliveryQueue.pending() {
			t.Fatal("closing a preceding contract stopped or drained the current delivery")
		}
		h.deliver(h.control, nil, closeSequencesMessageFrame("independent-control"))
		synctest.Wait()
		if h.delivered("independent-control") != 1 {
			t.Fatal("an independent receive sequence stalled behind pending delivery")
		}
	})
}
