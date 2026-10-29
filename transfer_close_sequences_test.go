// CloseSequences tests drive a provider-side client through its real receive
// and send buffers. Receive contracts are signed with the client's own provider
// key, the platform's CloseSequences arrives as a control-sourced Pack on its
// own receive sequence as it does from the platform, and each close report is
// read at the contract manager before its control send. Fake time advances only
// when a test sleeps, so an idle timeout never stands in for a close.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// One sender's receive sequence at the harness client.
type closeSequencesSender struct {
	sourceId       Id
	sequenceId     Id
	sequenceNumber uint64
}

// What one close report carries.
type closeSequencesReport struct {
	acked      uint64
	unacked    uint64
	checkpoint bool
}

type closeSequencesHarness struct {
	t       *testing.T
	client  *Client
	control *closeSequencesSender

	stateLock  sync.Mutex
	reports    []*protocol.CloseContract
	deliveries map[string]int
}

func newCloseSequencesHarness(t *testing.T, configure ...func(*ClientSettings)) *closeSequencesHarness {
	t.Helper()
	settings := closeWaitClientSettings()
	// fake time starts in 2000; contracts are required from every peer now
	settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
	for _, change := range configure {
		change(settings)
	}
	self := &closeSequencesHarness{
		t:          t,
		control:    &closeSequencesSender{sourceId: ControlId, sequenceId: NewId()},
		deliveries: map[string]int{},
	}
	self.client = NewClient(t.Context(), NewId(), NewNoContractClientOob(), settings)
	self.client.ContractManager().beforeOriginalCloseFrameForTest = func(report *protocol.CloseContract) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.reports = append(self.reports, proto.Clone(report).(*protocol.CloseContract))
	}
	self.client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
	})
	self.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_TestSimpleMessage {
				continue
			}
			var message protocol.SimpleMessage
			if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil {
				t.Error(err)
				continue
			}
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.deliveries[message.Content] += 1
			}()
		}
	})
	t.Cleanup(func() {
		if err := self.client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return self
}

func (self *closeSequencesHarness) newSender() *closeSequencesSender {
	return &closeSequencesSender{sourceId: NewId(), sequenceId: NewId()}
}

// A contract from a source to the harness client, signed with the client's own
// provider key as the platform signs it.
func (self *closeSequencesHarness) newReceiveContract(sourceId Id) (Id, *protocol.Frame) {
	self.t.Helper()
	contractId := NewId()
	storedContractBytes, err := proto.Marshal(&protocol.StoredContract{
		ContractId:        contractId.Bytes(),
		TransferByteCount: uint64(mib(1)),
		SourceId:          sourceId.Bytes(),
		DestinationId:     self.client.ClientId().Bytes(),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	contractManager := self.client.ContractManager()
	secret, ok := contractManager.GetProvideSecretKey(protocol.ProvideMode_Network)
	if !ok {
		self.t.Fatal("the client has no provider key")
	}
	return contractId, RequireToFrameWithDefaultProtocolVersion(&protocol.Contract{
		StoredContractBytes: storedContractBytes,
		StoredContractHmac:  SignStoredContract(contractManager.settings, secret, storedContractBytes),
		ProvideMode:         protocol.ProvideMode_Network,
	})
}

// Queues a contract from the harness client, as a CreateContractResult does,
// for the send sequence that takes contracts from the key.
func (self *closeSequencesHarness) queueSendContract(contractKey ContractKey) Id {
	self.t.Helper()
	contractId := NewId()
	storedContractBytes, err := proto.Marshal(&protocol.StoredContract{
		ContractId:        contractId.Bytes(),
		TransferByteCount: uint64(mib(1)),
		SourceId:          self.client.ClientId().Bytes(),
		DestinationId:     contractKey.Destination.DestinationId.Bytes(),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	err = self.client.ContractManager().addContract(contractKey, &protocol.Contract{
		StoredContractBytes: storedContractBytes,
		ProvideMode:         protocol.ProvideMode_Network,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	return contractId
}

// Delivers one Pack from a sender through the client's receive buffer, as the
// client's reader does. Returns what it debits from the sequence's contract.
func (self *closeSequencesHarness) deliver(
	sender *closeSequencesSender,
	contractFrame *protocol.Frame,
	frames ...*protocol.Frame,
) ByteCount {
	self.t.Helper()
	messageByteCount := MessageByteCount(frames)
	sequenceNumber := sender.sequenceNumber
	sender.sequenceNumber += 1
	receivePack := &ReceivePack{
		Source:     SourceId(sender.sourceId),
		SequenceId: sender.sequenceId,
		Pack: &protocol.Pack{
			MessageId:      NewId().Bytes(),
			SequenceId:     sender.sequenceId.Bytes(),
			SequenceNumber: sequenceNumber,
			Head:           sequenceNumber == 0,
			Frames:         frames,
			ContractFrame:  contractFrame,
		},
		ReceiveCallback:  self.client.receiveCallback,
		MessageByteCount: messageByteCount,
		Unwrapped:        true,
		EncryptionRole:   sequenceTlsRoleServer,
	}
	success, err := self.client.receiveBuffer.Pack(receivePack, time.Second)
	if err != nil || !success {
		receivePack.messagePoolReturn()
		self.t.Fatalf("pack admission = (%t, %v)", success, err)
	}
	return messageByteCount
}

// Delivers the platform's CloseSequences on the control sequence.
func (self *closeSequencesHarness) closeSequences(contractIds []Id, peerIds []Id) {
	self.t.Helper()
	self.deliver(self.control, nil, closeSequencesFrame(contractIds, peerIds))
}

// Sends one message to a destination; the send sequence keeps it unacknowledged.
func (self *closeSequencesHarness) send(destinationId Id, content string) {
	self.t.Helper()
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
	if !self.client.SendWithTimeout(frame, destinationId, func(error) {}, time.Second) {
		MessagePoolReturn(frame.MessageBytes)
		self.t.Fatal("send was not admitted")
	}
}

func (self *closeSequencesHarness) delivered(content string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.deliveries[content]
}

// The close reports for one contract, in order.
func (self *closeSequencesHarness) reportsFor(contractId Id) []closeSequencesReport {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	reports := []closeSequencesReport{}
	for _, report := range self.reports {
		if RequireIdFromBytes(report.ContractId) == contractId {
			reports = append(reports, closeSequencesReport{
				acked:      report.AckedByteCount,
				unacked:    report.UnackedByteCount,
				checkpoint: report.Checkpoint,
			})
		}
	}
	return reports
}

func (self *closeSequencesHarness) requireReports(contractId Id, expected ...closeSequencesReport) {
	self.t.Helper()
	reports := self.reportsFor(contractId)
	if len(reports) != len(expected) {
		self.t.Fatalf("contract %s reports = %+v, want %+v", contractId, reports, expected)
	}
	for i := range reports {
		if reports[i] != expected[i] {
			self.t.Fatalf("contract %s reports = %+v, want %+v", contractId, reports, expected)
		}
	}
}

// The receive sequences from one source that the buffer still holds.
func (self *closeSequencesHarness) receiveSequences(sourceId Id) []*ReceiveSequence {
	receiveBuffer := self.client.receiveBuffer
	receiveBuffer.mutex.Lock()
	defer receiveBuffer.mutex.Unlock()
	sequences := []*ReceiveSequence{}
	for id, sequence := range receiveBuffer.receiveSequences {
		if id.Source.SourceId == sourceId {
			sequences = append(sequences, sequence)
		}
	}
	return sequences
}

func (self *closeSequencesHarness) idleTimeout() time.Duration {
	return self.client.settings.ReceiveBufferSettings.IdleTimeout
}

func closeSequencesFrame(contractIds []Id, peerIds []Id) *protocol.Frame {
	closeSequences := &protocol.CloseSequences{}
	for _, contractId := range contractIds {
		closeSequences.ContractIds = append(closeSequences.ContractIds, contractId.Bytes())
	}
	for _, peerId := range peerIds {
		closeSequences.PeerIds = append(closeSequences.PeerIds, peerId.Bytes())
	}
	return RequireToFrameWithDefaultProtocolVersion(closeSequences)
}

func closeSequencesMessageFrame(content string) *protocol.Frame {
	return RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
}

// The other party has finished with the current contract of a live receive
// sequence. The sequence ends at once, without waiting for its idle timeout,
// and its exit reports a final with the sequence's counts rather than the
// checkpoint it leaves for a sender that may resume.
func TestCloseSequencesFinalizesCurrentReceiveContract(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness := newCloseSequencesHarness(t)
		sender := harness.newSender()
		contractId, contractFrame := harness.newReceiveContract(sender.sourceId)
		byteCount := harness.deliver(sender, contractFrame, closeSequencesMessageFrame("current-1"))
		byteCount += harness.deliver(sender, nil, closeSequencesMessageFrame("current-2"))
		synctest.Wait()
		if len(harness.receiveSequences(sender.sourceId)) != 1 || harness.delivered("current-2") != 1 {
			t.Fatal("the fixture did not start a receive sequence on its contract")
		}
		harness.requireReports(contractId)

		harness.closeSequences([]Id{contractId}, nil)
		synctest.Wait()
		harness.requireReports(contractId, closeSequencesReport{acked: uint64(byteCount)})
		if count := len(harness.receiveSequences(sender.sourceId)); count != 0 {
			t.Fatalf("%d receive sequences outlived the close of their current contract", count)
		}
	})
}

// The other party has finished with a contract that a live receive sequence
// has moved past but still holds open. Only that contract closes, with its own
// counts; the sequence keeps running on its current contract.
func TestCloseSequencesClosesOpenReceiveContractAndContinues(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness := newCloseSequencesHarness(t)
		sender := harness.newSender()
		previousContractId, previousFrame := harness.newReceiveContract(sender.sourceId)
		previousByteCount := harness.deliver(sender, previousFrame, closeSequencesMessageFrame("previous"))
		currentContractId, currentFrame := harness.newReceiveContract(sender.sourceId)
		currentByteCount := harness.deliver(sender, currentFrame, closeSequencesMessageFrame("current"))
		synctest.Wait()
		sequences := harness.receiveSequences(sender.sourceId)
		if len(sequences) != 1 {
			t.Fatalf("receive sequences = %d, want 1", len(sequences))
		}
		sequence := sequences[0]
		if sequence.receiveContract == nil || sequence.receiveContract.contractId != currentContractId ||
			sequence.openReceiveContracts[previousContractId] == nil {
			t.Fatal("the fixture did not leave the previous contract open behind the current one")
		}

		harness.closeSequences([]Id{previousContractId}, nil)
		synctest.Wait()
		harness.requireReports(previousContractId, closeSequencesReport{acked: uint64(previousByteCount)})
		harness.requireReports(currentContractId)
		if live := harness.receiveSequences(sender.sourceId); len(live) != 1 || live[0] != sequence {
			t.Fatal("closing a contract the sequence had moved past ended the sequence")
		}
		if _, open := sequence.openReceiveContracts[previousContractId]; open {
			t.Fatal("the closed contract stayed open in its sequence")
		}
		if sequence.openReceiveContracts[currentContractId] != sequence.receiveContract {
			t.Fatal("the close changed the sequence's current contract")
		}

		currentByteCount += harness.deliver(sender, nil, closeSequencesMessageFrame("after"))
		synctest.Wait()
		if harness.delivered("after") != 1 {
			t.Fatal("the sequence stopped delivering after the close")
		}
		time.Sleep(harness.idleTimeout() + time.Second)
		synctest.Wait()
		harness.requireReports(previousContractId, closeSequencesReport{acked: uint64(previousByteCount)})
		harness.requireReports(currentContractId, closeSequencesReport{
			acked:      uint64(currentByteCount),
			checkpoint: true,
		})
	})
}

// Nothing holds the contract any more: its sequence idled out and checkpointed
// it, or this client never held it. The client reports a final of (0, 0),
// which the server turns into a final with the checkpoint's count.
func TestCloseSequencesReportsZeroFinalForUnheldContract(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness := newCloseSequencesHarness(t, func(settings *ClientSettings) {
			// Keep the constructor worker live until its completion hook is installed.
			settings.ContractManagerSettings.ProvidePingTimeout = time.Hour
		})
		checkpointAcknowledged := make(chan struct{})
		var checkpointOnce sync.Once
		harness.client.ContractManager().beforeWorkerDoneForTest = func(name string) {
			if name == "contract close sync" {
				checkpointOnce.Do(func() { close(checkpointAcknowledged) })
			}
		}
		harness.holdControlAcknowledgements()()
		sender := harness.newSender()
		contractId, contractFrame := harness.newReceiveContract(sender.sourceId)
		byteCount := harness.deliver(sender, contractFrame, closeSequencesMessageFrame("idle"))
		synctest.Wait()
		time.Sleep(harness.idleTimeout() + time.Second)
		synctest.Wait()
		if count := len(harness.receiveSequences(sender.sourceId)); count != 0 {
			t.Fatalf("the fixture's receive sequence did not idle out: %d live", count)
		}
		checkpoint := closeSequencesReport{acked: uint64(byteCount), checkpoint: true}
		harness.requireReports(contractId, checkpoint)
		// Frame construction precedes native delivery. Wait for its actual
		// acknowledgement before exercising the already-delivered fallback.
		<-checkpointAcknowledged

		unknownContractId := NewId()
		harness.closeSequences([]Id{contractId, unknownContractId}, nil)
		synctest.Wait()
		harness.requireReports(contractId, checkpoint, closeSequencesReport{})
		harness.requireReports(unknownContractId, closeSequencesReport{})
	})
}

// A client removed through the api has finished with every contract. Its
// receive sequences end with finals, including the current contract, its send
// sequences end with finals, and contracts queued for it close with (0, 0).
func TestCloseSequencesRemovedPeerFinalizesReceiveAndSendWork(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness := newCloseSequencesHarness(t)
		peer := harness.newSender()
		receiveContractId, receiveFrame := harness.newReceiveContract(peer.sourceId)
		receiveByteCount := harness.deliver(peer, receiveFrame, closeSequencesMessageFrame("from-peer"))
		sendContractId := harness.queueSendContract(ContractKey{Destination: DestinationId(peer.sourceId)})
		harness.send(peer.sourceId, "to-peer")
		// queued under another key, so no live sequence takes it
		queuedContractId := harness.queueSendContract(ContractKey{
			Destination:       DestinationId(peer.sourceId),
			CompanionContract: true,
		})
		synctest.Wait()
		sendSequence := harness.client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: peer.sourceId}, nil)
		if sendSequence == nil || sendSequence.openSendContracts[sendContractId] == nil {
			t.Fatal("the fixture's send sequence did not take its contract")
		}
		if len(harness.receiveSequences(peer.sourceId)) != 1 || harness.delivered("from-peer") != 1 {
			t.Fatal("the fixture did not start a receive sequence from the peer")
		}
		harness.requireReports(receiveContractId)
		harness.requireReports(sendContractId)
		harness.requireReports(queuedContractId)

		harness.closeSequences(nil, []Id{peer.sourceId})
		synctest.Wait()
		harness.requireReports(receiveContractId, closeSequencesReport{acked: uint64(receiveByteCount)})
		sendReports := harness.reportsFor(sendContractId)
		if len(sendReports) != 1 || sendReports[0].checkpoint || sendReports[0].acked != 0 || sendReports[0].unacked == 0 {
			t.Fatalf("send contract reports = %+v, want one final with the unacknowledged message", sendReports)
		}
		harness.requireReports(queuedContractId, closeSequencesReport{})
		if count := len(harness.receiveSequences(peer.sourceId)); count != 0 {
			t.Fatalf("%d receive sequences from the removed peer remain", count)
		}
		if harness.client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: peer.sourceId}, nil) != nil {
			t.Fatal("the send sequence to the removed peer remains")
		}
	})
}

// A live send sequence holds the listed contract. The sequence is canceled, and
// its exit reports the contract's final with its counts.
func TestCloseSequencesCancelsSendSequenceHoldingContract(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness := newCloseSequencesHarness(t)
		destinationId := NewId()
		sendContractId := harness.queueSendContract(ContractKey{Destination: DestinationId(destinationId)})
		harness.send(destinationId, "to-destination")
		synctest.Wait()
		sendSequence := harness.client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destinationId}, nil)
		if sendSequence == nil || sendSequence.openSendContracts[sendContractId] == nil {
			t.Fatal("the fixture's send sequence did not take its contract")
		}

		harness.closeSequences([]Id{sendContractId}, nil)
		synctest.Wait()
		sendReports := harness.reportsFor(sendContractId)
		if len(sendReports) != 1 || sendReports[0].checkpoint || sendReports[0].acked != 0 || sendReports[0].unacked == 0 {
			t.Fatalf("send contract reports = %+v, want one final with the unacknowledged message", sendReports)
		}
		if harness.client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destinationId}, nil) != nil {
			t.Fatal("the send sequence holding the closed contract remains")
		}
	})
}

// The listed contract waits queued as a live send sequence's successor. It
// leaves the queue with a final of (0, 0), so no sequence can take it, and the
// sequence that would take it is canceled; its exit reports its open
// contract's final.
func TestCloseSequencesCancelsSendSequenceOfQueuedSuccessor(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness := newCloseSequencesHarness(t)
		destinationId := NewId()
		contractKey := ContractKey{Destination: DestinationId(destinationId)}
		sendContractId := harness.queueSendContract(contractKey)
		harness.send(destinationId, "to-destination")
		synctest.Wait()
		sendSequence := harness.client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destinationId}, nil)
		if sendSequence == nil || sendSequence.openSendContracts[sendContractId] == nil {
			t.Fatal("the fixture's send sequence did not take its contract")
		}
		successorContractId := harness.queueSendContract(contractKey)
		synctest.Wait()
		if sendSequence.openSendContracts[successorContractId] != nil {
			t.Fatal("the fixture's successor was taken rather than left queued")
		}

		harness.closeSequences([]Id{successorContractId}, nil)
		synctest.Wait()
		harness.requireReports(successorContractId, closeSequencesReport{})
		sendReports := harness.reportsFor(sendContractId)
		if len(sendReports) != 1 || sendReports[0].checkpoint || sendReports[0].unacked == 0 {
			t.Fatalf("send contract reports = %+v, want one final with the unacknowledged message", sendReports)
		}
		if harness.client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destinationId}, nil) != nil {
			t.Fatal("the send sequence that would take the closed successor remains")
		}
	})
}

// Only the platform may ask. A peer that sends CloseSequences on its own
// sequence closes nothing, and the same request from the platform then closes
// the contract.
func TestCloseSequencesIgnoresNonControlSource(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness := newCloseSequencesHarness(t)
		sender := harness.newSender()
		contractId, contractFrame := harness.newReceiveContract(sender.sourceId)
		byteCount := harness.deliver(sender, contractFrame, closeSequencesMessageFrame("data"))
		byteCount += harness.deliver(
			sender,
			nil,
			closeSequencesFrame([]Id{contractId}, []Id{sender.sourceId}),
			closeSequencesMessageFrame("after-request"),
		)
		synctest.Wait()
		if harness.delivered("after-request") != 1 {
			t.Fatal("the peer's request was not delivered to the client's callbacks")
		}
		harness.requireReports(contractId)
		if count := len(harness.receiveSequences(sender.sourceId)); count != 1 {
			t.Fatalf("a peer's close request changed its receive sequences: %d live", count)
		}

		harness.closeSequences([]Id{contractId}, nil)
		synctest.Wait()
		harness.requireReports(contractId, closeSequencesReport{acked: uint64(byteCount)})
	})
}

// Without the platform's request nothing changes. An idle exit reports a final
// for the contract its sequence moved past and checkpoints the current one,
// which the sender may resume.
func TestCloseSequencesAbsentIdleExitCheckpointsCurrentContract(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness := newCloseSequencesHarness(t)
		sender := harness.newSender()
		previousContractId, previousFrame := harness.newReceiveContract(sender.sourceId)
		previousByteCount := harness.deliver(sender, previousFrame, closeSequencesMessageFrame("previous"))
		currentContractId, currentFrame := harness.newReceiveContract(sender.sourceId)
		currentByteCount := harness.deliver(sender, currentFrame, closeSequencesMessageFrame("current"))
		synctest.Wait()
		harness.requireReports(previousContractId)
		harness.requireReports(currentContractId)

		time.Sleep(harness.idleTimeout() + time.Second)
		synctest.Wait()
		if count := len(harness.receiveSequences(sender.sourceId)); count != 0 {
			t.Fatalf("the receive sequence did not idle out: %d live", count)
		}
		harness.requireReports(previousContractId, closeSequencesReport{acked: uint64(previousByteCount)})
		harness.requireReports(currentContractId, closeSequencesReport{
			acked:      uint64(currentByteCount),
			checkpoint: true,
		})
	})
}

// The provider's source retirement, the existing caller of the receive
// buffer's source cancel, keeps its checkpoint for the current contract.
func TestCloseSequencesSourceCancelStillCheckpointsCurrentContract(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness := newCloseSequencesHarness(t)
		sender := harness.newSender()
		contractId, contractFrame := harness.newReceiveContract(sender.sourceId)
		byteCount := harness.deliver(sender, contractFrame, closeSequencesMessageFrame("retired"))
		synctest.Wait()

		harness.client.receiveBuffer.cancelSourceAndWait(sender.sourceId)
		synctest.Wait()
		harness.requireReports(contractId, closeSequencesReport{
			acked:      uint64(byteCount),
			checkpoint: true,
		})
	})
}

// The worker may wait on a removed peer's sequences; the control callback must
// not. A request that arrives meanwhile is recorded up to the pending bound,
// the rest is dropped, and the callbacks behind it keep delivering. Once the
// wait ends the recorded ids close, and a dropped id is admitted when the
// platform sends it again.
func TestCloseSequencesReceiveStaysLiveWhileWorkerWaits(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		peerId := NewId()
		exitEntered := make(chan struct{})
		exitRelease := make(chan struct{})
		var enteredOnce sync.Once
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() { close(exitRelease) })
		}
		harness := newCloseSequencesHarness(t, func(settings *ClientSettings) {
			settings.CloseSequencesMaxPendingCount = 2
			settings.ReceiveBufferSettings.beforeAckWorkerStopForTest = func(id receiveSequenceId) {
				if id.Source.SourceId == peerId {
					enteredOnce.Do(func() { close(exitEntered) })
					<-exitRelease
				}
			}
		})
		// before the harness joins the client
		t.Cleanup(release)
		peer := &closeSequencesSender{sourceId: peerId, sequenceId: NewId()}
		peerContractId, peerFrame := harness.newReceiveContract(peerId)
		peerByteCount := harness.deliver(peer, peerFrame, closeSequencesMessageFrame("from-peer"))
		synctest.Wait()

		harness.closeSequences(nil, []Id{peerId})
		synctest.Wait()
		select {
		case <-exitEntered:
		default:
			t.Fatal("the removed peer's receive sequence did not begin its exit")
		}

		contractIds := []Id{NewId(), NewId(), NewId()}
		harness.deliver(
			harness.control,
			nil,
			closeSequencesFrame(contractIds, nil),
			closeSequencesMessageFrame("control-after"),
		)
		synctest.Wait()
		if harness.delivered("control-after") != 1 {
			t.Fatal("the control callbacks stalled behind the close worker")
		}
		for _, contractId := range contractIds {
			harness.requireReports(contractId)
		}

		release()
		synctest.Wait()
		harness.requireReports(peerContractId, closeSequencesReport{acked: uint64(peerByteCount)})
		harness.requireReports(contractIds[0], closeSequencesReport{})
		harness.requireReports(contractIds[1], closeSequencesReport{})
		harness.requireReports(contractIds[2])

		harness.closeSequences([]Id{contractIds[2]}, nil)
		synctest.Wait()
		harness.requireReports(contractIds[2], closeSequencesReport{})
	})
}

// A close request accepted before the exit turns the current contract's
// checkpoint into a final; the contract it moved past gets its final as
// always. Once the exit has reported, a request is refused, so the close
// worker reports the close itself.
func TestReceiveSequenceCloseRequestBeforeExitFinalizesAndAfterIsRefused(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		h := newReceiveContractBoundHarness(t)
		h.rotate(2, false)
		current := h.sequence.receiveContract
		h.debit(current, true, 11, 13)
		if !h.sequence.requestCloseContract(current.contractId) {
			t.Fatal("a live sequence refused a close request")
		}

		// the harness expects a final for every contract, the current one too
		h.closed = true
		h.sequence.Cancel()
		h.sequence.Run()
		h.sequence.Close()
		h.requirePosts(true)
		if h.sequence.requestCloseContract(current.contractId) {
			t.Fatal("a sequence that has reported its contracts accepted a close request")
		}
	})
}
