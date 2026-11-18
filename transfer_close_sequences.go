// The platform's CloseSequences (server CLIENTCLOSE.md). When the other party
// of a contract has sent its final close, or a client was removed through the
// api, the platform asks this client to report its own final for the listed
// contracts now, instead of leaving them to the platform's expiry sweep.
package connect

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect/protocol"
)

// sequenceCloser applies the platform's CloseSequences to this client's
// sequences.
//
// Receive runs on the control sequence's goroutine, so it only records the ids,
// up to `CloseSequencesMaxPendingCount`, and wakes one client-owned worker. The
// worker never touches a sequence's contract state: a receive sequence applies
// a request on its own goroutine or at its exit, and a send sequence is
// canceled, so its exit reports finals as it does today.
//
// For each contract id:
//   - A live receive sequence holds it open behind its current contract: the
//     sequence reports a final with its counts, drops it and keeps running.
//   - It is a live receive sequence's current contract: the sequence ends, and
//     its exit reports a final for it rather than a checkpoint.
//   - A live send sequence has taken it, or it waits queued for one: the send
//     sequence is canceled and its exit reports the finals. A queued contract
//     leaves its queue with a final of (0, 0).
//   - Nothing holds it: a final of (0, 0). Its counts went out before, for
//     example in the checkpoint at its sequence's exit, and the platform turns
//     that checkpoint into a final with the same count.
//
// For each peer id, a client removed through the api: every receive sequence
// from it ends with finals for all of its contracts, every send sequence to it
// is canceled, and contracts queued for it close with (0, 0).
//
// Contracts go before peers, so a contract that a listed peer's sequence holds
// gets that sequence's counts in its final rather than a (0, 0) after the
// sequence has ended.
//
// Safe for concurrent use.
type sequenceCloser struct {
	ctx    context.Context
	client *Client
	worker *coalescingCallbackWorker

	maxPendingCount int

	stateLock          sync.Mutex
	pendingContractIds []Id
	pendingPeerIds     []Id

	// ids dropped past the pending bound
	droppedCount atomic.Uint64
}

// Starts the worker, which runs until the client closes. The client's buffers
// and contract manager are read only on the worker, after construction.
func newSequenceCloser(client *Client) *sequenceCloser {
	sequenceCloser := &sequenceCloser{
		ctx:             client.ctx,
		client:          client,
		maxPendingCount: client.settings.CloseSequencesMaxPendingCount,
	}
	sequenceCloser.worker = newCoalescingCallbackWorker(client.ctx, sequenceCloser.closePending)
	return sequenceCloser
}

// ReceiveFunction
func (self *sequenceCloser) Receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	if !source.IsControlSource() {
		return
	}
	for _, frame := range frames {
		if frame.MessageType != protocol.MessageType_TransferCloseSequences {
			continue
		}
		message, err := FromFrame(frame)
		if err != nil {
			continue
		}
		if closeSequences, ok := message.(*protocol.CloseSequences); ok {
			self.add(closeSequences)
		}
	}
}

// Records one message's ids for the worker and wakes it. Ids past the pending
// bound are dropped and counted.
func (self *sequenceCloser) add(closeSequences *protocol.CloseSequences) {
	added := false
	dropped := 0
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		for _, contractIdBytes := range closeSequences.ContractIds {
			contractId, err := IdFromBytes(contractIdBytes)
			if err != nil {
				continue
			}
			if self.maxPendingCount <= len(self.pendingContractIds)+len(self.pendingPeerIds) {
				dropped += 1
				continue
			}
			self.pendingContractIds = append(self.pendingContractIds, contractId)
			added = true
		}
		for _, peerIdBytes := range closeSequences.PeerIds {
			peerId, err := IdFromBytes(peerIdBytes)
			if err != nil {
				continue
			}
			if self.maxPendingCount <= len(self.pendingContractIds)+len(self.pendingPeerIds) {
				dropped += 1
				continue
			}
			self.pendingPeerIds = append(self.pendingPeerIds, peerId)
			added = true
		}
	}()

	if 0 < dropped {
		droppedCount := self.droppedCount.Add(uint64(dropped))
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[close]%s drop %d close ids past the pending bound (%d total)\n", self.client.ClientTag(), dropped, droppedCount)
		}
	}
	if added {
		self.worker.Dispatch()
	}
}

// Applies the pending ids. Runs on the worker.
func (self *sequenceCloser) closePending() {
	var contractIds []Id
	var peerIds []Id
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		contractIds = self.pendingContractIds
		peerIds = self.pendingPeerIds
		self.pendingContractIds = nil
		self.pendingPeerIds = nil
	}()

	if 0 < len(contractIds) {
		self.closeContracts(contractIds)
	}
	for _, peerId := range peerIds {
		select {
		case <-self.ctx.Done():
			return
		default:
		}
		self.closePeer(peerId)
	}
}

// Each contract goes to the receive sequence that holds it, then to the send
// side, and otherwise reports a final of (0, 0).
func (self *sequenceCloser) closeContracts(contractIds []Id) {
	unheldContractIds := []Id{}
	seenContractIds := map[Id]bool{}
	for _, contractId := range contractIds {
		if seenContractIds[contractId] {
			continue
		}
		seenContractIds[contractId] = true
		receiveSequence := self.client.receiveBuffer.contractHolders.find(contractId)
		if receiveSequence != nil && receiveSequence.requestCloseContract(contractId) {
			continue
		}
		unheldContractIds = append(unheldContractIds, contractId)
	}
	if len(unheldContractIds) == 0 {
		return
	}

	contractManager := self.client.ContractManager()
	openKeys, queuedKeys := contractManager.takeSendContracts(unheldContractIds)
	sendSequenceIds := map[sendSequenceId]bool{}
	for _, contractKey := range openKeys {
		sendSequenceIds[contractKey.sendSequenceId()] = true
	}
	for _, contractKey := range queuedKeys {
		sendSequenceIds[contractKey.sendSequenceId()] = true
	}
	canceledSendSequenceIds := self.client.sendBuffer.cancelSendSequences(sendSequenceIds)
	for _, contractId := range unheldContractIds {
		if contractKey, ok := openKeys[contractId]; ok && canceledSendSequenceIds[contractKey.sendSequenceId()] {
			// the canceled sequence's exit reports the final
			continue
		}
		// nothing holds it, or it has just left its queue
		contractManager.requestContractClose(contractId)
	}
}

// Ends this client's work with a client removed through the api. Its receive
// sequences end first, as the provider's source retirement orders them, then
// its send sequences, whose exits flush their own queues; the flush after
// them reaches queues that no live sequence owns.
func (self *sequenceCloser) closePeer(peerId Id) {
	if peerId == ControlId || peerId == self.client.ClientId() {
		return
	}
	self.client.receiveBuffer.closeSourceAndWait(peerId)
	self.client.sendBuffer.cancelDestinationAndWait(peerId)
	self.client.ContractManager().flushDestinationContractQueues(peerId)
}

// Stops the worker without waiting. Pending ids are dropped, and the client's
// own shutdown checkpoints what its sequences hold.
func (self *sequenceCloser) close() {
	if self == nil {
		return
	}
	self.worker.Close()
}

// Joins the worker, or returns when ctx expires.
func (self *sequenceCloser) wait(ctx context.Context) error {
	if self == nil {
		return nil
	}
	return waitForLifecycleDone(ctx, self.worker.done, "close sequences worker")
}

// receiveContractHolders indexes each open receive contract by the live
// sequence that holds it, so the platform's close requests find their
// sequence without reading sequence-owned state. A sequence adds and removes
// only its own entries, from its own goroutine. A newer sequence that resumes
// a contract takes over its entry, and the older sequence's removal then
// leaves the newer entry in place.
//
// Safe for concurrent use. A nil index records nothing.
type receiveContractHolders struct {
	stateLock         sync.Mutex
	contractSequences map[Id]*ReceiveSequence
}

func newReceiveContractHolders() *receiveContractHolders {
	return &receiveContractHolders{
		contractSequences: map[Id]*ReceiveSequence{},
	}
}

// Records the sequence as the contract's holder.
func (self *receiveContractHolders) hold(contractId Id, receiveSequence *ReceiveSequence) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.contractSequences[contractId] = receiveSequence
}

// Removes the sequence's entry. A newer holder's entry stays.
func (self *receiveContractHolders) release(contractId Id, receiveSequence *ReceiveSequence) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.contractSequences[contractId] == receiveSequence {
		delete(self.contractSequences, contractId)
	}
}

// The live sequence holding the contract, or nil.
func (self *receiveContractHolders) find(contractId Id) *ReceiveSequence {
	if self == nil {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.contractSequences[contractId]
}

// Asks the sequence to report a final for one of its open contracts, because
// the other party has finished with it. Run reports it and drops it, or, for
// the current contract, ends the sequence so its exit reports it. False once
// the exit has reported the sequence's contracts: the caller then reports the
// close itself.
func (self *ReceiveSequence) requestCloseContract(contractId Id) bool {
	accepted := func() bool {
		self.closeStateLock.Lock()
		defer self.closeStateLock.Unlock()

		if self.closeRequestsTaken {
			return false
		}
		if self.closeContractIds == nil {
			self.closeContractIds = map[Id]bool{}
		}
		self.closeContractIds[contractId] = true
		return true
	}()
	if accepted {
		select {
		case self.closeNotify <- struct{}{}:
		default:
		}
	}
	return accepted
}

// Ends the sequence with finals for all of its contracts, the current one
// included, because the other party has finished with them. Does not wait.
func (self *ReceiveSequence) closeContractsAndCancel() {
	var reportedContractId *Id
	func() {
		self.closeStateLock.Lock()
		defer self.closeStateLock.Unlock()

		self.closeCurrentContract = true
		if self.closeRequestsTaken && self.receiveContract != nil {
			contractId := self.receiveContract.contractId
			reportedContractId = &contractId
		}
	}()
	if reportedContractId != nil {
		self.client.ContractManager().requestContractClose(*reportedContractId)
	}
	self.Cancel()
}

// Takes the pending close requests on Run's goroutine. An open contract other
// than the current one reports its final with its counts and leaves the
// sequence, which keeps running. True when the current contract is closed: the
// sequence ends, and its exit reports that final.
func (self *ReceiveSequence) applyCloseRequests() bool {
	var contractIds map[Id]bool
	closeCurrentContract := false
	func() {
		self.closeStateLock.Lock()
		defer self.closeStateLock.Unlock()

		contractIds = self.closeContractIds
		self.closeContractIds = nil
		if self.receiveContract != nil && contractIds[self.receiveContract.contractId] {
			self.closeCurrentContract = true
		}
		closeCurrentContract = self.closeCurrentContract
	}()

	for contractId := range contractIds {
		receiveContract, ok := self.openReceiveContracts[contractId]
		if !ok || receiveContract == self.receiveContract {
			// the overflow trim already reported it, or the exit reports it
			continue
		}
		if receiveContract.unackedByteCount != 0 {
			self.log.Infof("[r]%s<-%s s(%s) close contract with unacked =  %d\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, receiveContract.unackedByteCount)
		}
		self.client.ContractManager().CloseContract(
			receiveContract.contractId,
			receiveContract.ackedByteCount,
			receiveContract.unackedByteCount,
		)
		delete(self.openReceiveContracts, contractId)
		self.contractHolders.release(contractId, self)
	}
	if closeCurrentContract && self.log.V(1).Enabled() {
		self.log.Infof("[r]%s<-%s s(%s) exit current contract closed\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
	}
	return closeCurrentContract
}

// Takes requests already present at exit. Later requests remain accepted until
// every counted report has been handed to the contract manager.
func (self *ReceiveSequence) takeCloseRequests() bool {
	self.closeStateLock.Lock()
	defer self.closeStateLock.Unlock()

	closeCurrentContract := self.closeCurrentContract
	if self.receiveContract != nil && self.closeContractIds[self.receiveContract.contractId] {
		closeCurrentContract = true
	}
	self.closeContractIds = nil
	return closeCurrentContract
}

// Hands requests racing report creation to the manager before releasing the
// holder index. Its pending reports order any needed zero final after counts.
func (self *ReceiveSequence) finishCloseRequests() {
	var contractIdKVs map[Id]bool
	func() {
		self.closeStateLock.Lock()
		defer self.closeStateLock.Unlock()
		self.closeRequestsTaken = true
		contractIdKVs = self.closeContractIds
		self.closeContractIds = nil
		if self.closeCurrentContract && self.receiveContract != nil {
			if contractIdKVs == nil {
				contractIdKVs = map[Id]bool{}
			}
			contractIdKVs[self.receiveContract.contractId] = true
		}
	}()
	for contractId := range contractIdKVs {
		self.client.ContractManager().requestContractClose(contractId)
	}
}
