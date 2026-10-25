// An explicitly failed unanswered SYN can retry from bounded source-owned
// bytes, without waiting for another native RTO. The existing parent reaper
// dispatches work; no packet, timer or goroutine is created by a failure callback.
package connect

import (
	"bytes"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

// Original metadata is immutable after publication. A retry carries only a
// numeric owner token and the existing flow, never the cached packet owner.
type tcpSynRetryRequest struct {
	source      TransferPath
	provideMode protocol.ProvideMode
	pin         flowPin
	update      *multiClientChannelUpdate
	serial      uint64
}

// The flow lock guards cache/queue references. Packet and credit remain live
// until both the cache and queued/in-flight dispatcher have released them.
// Queue links use the parent lock; release links use the flow lock.
type tcpSynRetry struct {
	update      *multiClientChannelUpdate
	packet      []byte
	credit      *transferRetainedControlCredit
	source      TransferPath
	provideMode protocol.ProvideMode
	pin         flowPin
	sequence    uint32
	serial      uint64
	references  uint32
	queued      bool
	next        *tcpSynRetry
	released    *tcpSynRetry
}

func (self *parsedPacketGroup) isSynRetry() bool {
	return self.synRetry != nil && self.synRetry.serial != 0
}

func (self *parsedPacketGroup) prepareSynRetry(source TransferPath, mode protocol.ProvideMode, parent *RemoteUserNatMultiClient, update *multiClientChannelUpdate, path *IpPath) {
	if path == nil || path.Protocol != IpProtocolTcp || !path.Syn || path.Ack || self.synRetry != nil || parent.flowReaperDone == nil {
		return
	}
	if update.receivedInbound.Load() {
		update.stateLock.Lock()
		established := update.receivedInbound.Load() && update.synGenerationSeen && update.synGenerationNumber == path.SequenceNumber
		update.stateLock.Unlock()
		if established {
			return
		}
	}
	self.synRetry = &tcpSynRetryRequest{source: source, provideMode: mode, pin: self.pin}
}

func (self *tcpSynRetry) validWithLock(update *multiClientChannelUpdate) bool {
	return self != nil && self.update == update && update.synRetry == self &&
		!update.IsDone() && !update.sourceReset.Load() && !update.receivedInbound.Load() &&
		!update.affinitySources.closed && !update.pendingCollapseResetWithLock() &&
		!update.pendingCollapseCloseWithLock() &&
		(!update.synGenerationSeen || update.synGenerationNumber == self.sequence)
}

func (self *parsedPacketGroup) validSynRetryWithLock(update *multiClientChannelUpdate) bool {
	return !self.isSynRetry() || self.synRetry.update == update && update.synRetry != nil &&
		update.synRetry.serial == self.synRetry.serial && update.synRetry.validWithLock(update)
}

// Materialization has already published source control. These actual borrowed
// frames still own their bytes in v1 and raw encodings, unlike an old parsed
// descriptor's packet slice after the legacy public caller returned it.
func (self sendGroupDisposition) retainSynRetry(source *SendSequence, frames []*protocol.Frame) {
	group, ok := self.target.(*parsedPacketGroup)
	if !ok || group.synRetry == nil || group.collapseAdmission.update == nil ||
		self.end-self.start != len(frames) || group.completionFlags.Load()&groupCompletionAbandoned != 0 {
		return
	}
	update := group.collapseAdmission.update
	defer update.releaseSynRetryMemory()
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	if update.IsDone() || update.receivedInbound.Load() || update.affinitySources.closed ||
		group.completionFlags.Load()&groupCompletionAbandoned != 0 || !group.validSynRetryWithLock(update) {
		return
	}
	for _, frame := range frames {
		if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
			continue
		}
		packet, err := ipPacketToProviderBytes(frame)
		if err != nil {
			continue
		}
		path, err := ParseIpPath(packet)
		if err != nil || path.Protocol != IpProtocolTcp {
			continue
		}
		if path.Rst || path.Fin {
			update.clearSynRetryWithLock()
			continue
		}
		if !path.Syn || path.Ack {
			continue
		}
		if retained := update.synRetry; retained != nil && retained.sequence == path.SequenceNumber && bytes.Equal(retained.packet, packet) {
			continue
		}
		// A newly admitted different SYN cannot borrow the preceding packet,
		// even when this bounded extra reservation cannot be obtained.
		update.clearSynRetryWithLock()
		if update.synRetrySerial == ^uint64(0) {
			continue
		}
		metadata := ByteCount(unsafe.Sizeof(tcpSynRetry{})) + ByteCount(len(group.synRetry.pin.appId))
		metadata = (metadata + 63) / 64 * 64
		credit, reserved := source.reserveRetainedControlMemory(addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(len(packet))), metadata))
		if !reserved {
			continue
		}
		copyPacket := MessagePoolCopy(packet)
		update.synRetrySerial++
		update.synRetry = &tcpSynRetry{
			update: update, packet: copyPacket, credit: credit,
			source: group.synRetry.source, provideMode: group.synRetry.provideMode,
			pin: group.synRetry.pin, sequence: path.SequenceNumber,
			serial: update.synRetrySerial, references: 1,
		}
	}
}

func (self *multiClientChannelUpdate) releaseSynRetryReferenceWithLock(owner *tcpSynRetry) {
	if owner.references == 0 {
		panic("SYN retry owner lost its reference")
	}
	owner.references--
	if owner.references == 0 {
		owner.released = self.synRetryReleased.Load()
		self.synRetryReleased.Store(owner)
	}
}

func (self *multiClientChannelUpdate) clearSynRetryWithLock() {
	if owner := self.synRetry; owner != nil {
		self.synRetry = nil
		self.releaseSynRetryReferenceWithLock(owner)
	}
}

// Releasing credit can notify other senders, so it never runs under a flow
// or parent lock. The final owner drops every packet/graph reference first.
func (self *multiClientChannelUpdate) releaseSynRetryMemory() {
	if self.synRetryReleased.Load() == nil {
		return
	}
	self.stateLock.Lock()
	owners := self.synRetryReleased.Swap(nil)
	self.stateLock.Unlock()
	for owners != nil {
		owner := owners
		owners, owner.released = owner.released, nil
		packet, credit := owner.packet, owner.credit
		owner.packet, owner.credit, owner.update, owner.next = nil, nil, nil, nil
		MessagePoolReturn(packet)
		credit.Release()
	}
}

// Only a last live explicit failure reaches here. Repeated signals coalesce;
// no provider, capacity or established-flow decision is inferred by this queue.
func (self *RemoteUserNatMultiClient) enqueueSynRetry(update *multiClientChannelUpdate) {
	if update == nil || self.flowReaperDone == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.ctx.Err() != nil || self.affinityUpdateWithLock(update.ipPath) != update || self.affinityTransitions[update] != nil {
		return
	}
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	owner := update.synRetry
	if owner == nil || !owner.validWithLock(update) || owner.queued || update.client.Load() != nil || update.race != nil {
		return
	}
	owner.references++
	owner.queued, owner.next = true, nil
	if self.synRetryTail == nil {
		self.synRetryHead = owner
	} else {
		self.synRetryTail.next = owner
	}
	self.synRetryTail = owner
	self.notifyFlowReaper()
}

func (self *RemoteUserNatMultiClient) takeSynRetry() *tcpSynRetry {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	owner := self.synRetryHead
	if owner != nil {
		self.synRetryHead, owner.next = owner.next, nil
		if self.synRetryHead == nil {
			self.synRetryTail = nil
		}
	}
	return owner
}

func (self *RemoteUserNatMultiClient) finishSynRetry(owner *tcpSynRetry) {
	update := owner.update
	update.stateLock.Lock()
	update.releaseSynRetryReferenceWithLock(owner)
	update.stateLock.Unlock()
	update.releaseSynRetryMemory()
}

// Work present at entry is a finite, byte-charged batch. Newly generated
// failures wake the next pass instead of extending this one indefinitely.
func (self *RemoteUserNatMultiClient) drainSynRetries() {
	self.stateLock.Lock()
	last := self.synRetryTail
	self.stateLock.Unlock()
	for last != nil && self.ctx.Err() == nil {
		owner := self.takeSynRetry()
		if owner == nil {
			return
		}
		func() {
			defer self.finishSynRetry(owner)
			update := owner.update
			update.stateLock.Lock()
			owner.queued = false
			eligible := owner.validWithLock(update) && update.client.Load() == nil && update.race == nil
			update.stateLock.Unlock()
			if !eligible {
				return
			}
			if hook := self.beforeSynRetryDispatchForTest; hook != nil {
				hook(update)
			}
			self.dispatchSynRetry(owner)
		}()
		if owner == last {
			return
		}
	}
}

func (self *RemoteUserNatMultiClient) discardSynRetries() {
	for owner := self.takeSynRetry(); owner != nil; owner = self.takeSynRetry() {
		owner.update.stateLock.Lock()
		owner.queued = false
		owner.update.stateLock.Unlock()
		self.finishSynRetry(owner)
	}
}

func (self *RemoteUserNatMultiClient) dispatchSynRetry(owner *tcpSynRetry) {
	update := owner.update
	update.stateLock.Lock()
	if !owner.validWithLock(update) || update.client.Load() != nil || update.race != nil || self.ctx.Err() != nil {
		update.stateLock.Unlock()
		return
	}
	packet := MessagePoolShareReadOnly(owner.packet)
	update.stateLock.Unlock()
	path, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		MessagePoolReturn(packet)
		return
	}
	request := &tcpSynRetryRequest{source: owner.source, provideMode: owner.provideMode, pin: owner.pin, update: update, serial: owner.serial}
	group := &parsedPacketGroup{
		ipPath: path, pin: owner.pin, byteCount: ByteCount(len(packet)), synRetry: request,
		packets: []parsedPacket{{packet: packet, ipPath: path, payload: payload, pin: owner.pin}},
	}
	if !self.sendParsedPacketGroupAttempt(owner.source, owner.provideMode, group, 0, true, nil) {
		MessagePoolReturn(packet)
	}
}

// A retry belongs to this existing canonical flow. It must not create a new
// tuple generation, renew affinity, or borrow a newly established selection.
func (self *RemoteUserNatMultiClient) sendOwnedSynRetryPath(group *parsedPacketGroup, timeout time.Duration, callback func(*multiClientChannelUpdate, *multiClientChannel, time.Duration)) {
	update := group.synRetry.update
	self.stateLock.Lock()
	current := self.affinityUpdateWithLock(group.ipPath)
	valid := self.ctx.Err() == nil && current == update && self.affinityTransitions[update] == nil
	self.stateLock.Unlock()
	if !valid {
		return
	}
	callback(update, update.client.Load(), timeout)
	if client := update.client.Load(); client != nil {
		self.bindClientFlow(update, client)
	}
}
