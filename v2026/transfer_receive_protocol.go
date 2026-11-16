// Protocol originals retain their existing reliable identity and lifecycle.
// Their handoff preserves the existing configured sequence count independently
// of DATA admission. Future originals retain their distinct ordered ownership;
// no protocol byte budget or on-wire exemption exists.
package connect

import (
	"errors"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/encoding/protowire"
)

// Uses the effective protobuf scalar values without allocating a decode graph.
func protocolOnlyWireFrame(wire []byte) bool {
	kind, raw := protocol.MessageType_TransferPack, false
	var body []byte
	cursor := receiveMemoryWireCursor{bytes: wire}
	for cursor.offset < len(wire) {
		field, ok := cursor.next()
		if !ok {
			return false
		}
		switch field.number {
		case 1:
			if field.wireType != protowire.VarintType {
				return false
			}
			kind = protocol.MessageType(int32(field.value))
		case 2:
			if field.wireType != protowire.BytesType {
				return false
			}
			body = field.bytes
		case 3:
			if field.wireType != protowire.VarintType {
				return false
			}
			raw = field.value != 0
		}
	}
	return protocolOnlyFrame(kind, raw, body)
}

// No mutable DATA-admission mutex or byte reservation is acquired here.
// A distinct ordered original is never overwritten or falsely acknowledged.
func (self *ReceiveSequence) packProtocol(pack *ReceivePack, timeout time.Duration) (bool, error) {
	if timeout == 0 {
		if !self.protocolPackMutex.TryLock() {
			return false, nil
		}
	} else {
		self.protocolPackMutex.Lock()
	}
	defer self.protocolPackMutex.Unlock()
	if self.ctx.Err() != nil || !self.idleCondition.UpdateOpen() {
		return false, errors.New("Done.")
	}
	defer self.idleCondition.UpdateClose()
	if !self.bindReceiveMemoryOwner(pack) {
		return false, nil
	}
	select {
	case <-pack.carrierDone:
		return false, errors.New("receive carrier closed")
	default:
	}
	select {
	case self.protocolPacks <- pack:
		return true, nil
	default:
	}
	if timeout == 0 {
		return false, nil
	}
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	case <-pack.carrierDone:
		return false, errors.New("receive carrier closed")
	case self.protocolPacks <- pack:
		return true, nil
	case <-deadline:
		return false, nil
	}
}

// Teardown owns the protocol mailbox only after admission has unwound.
func (self *ReceiveSequence) closeProtocolPacks() {
	self.protocolPackMutex.Lock()
	defer self.protocolPackMutex.Unlock()
	if self.protocolPacks == nil {
		return
	}
	close(self.protocolPacks)
	for pack := range self.protocolPacks {
		pack.messagePoolReturn()
	}
}

// DATA pressure can reclaim only DATA custody. A protocol tail frees no
// DATA credit and keeps its identity whether tentative or acknowledged.
func (self *ReceiveSequence) lastDataHeldItem() *receiveItem {
	queue := self.receiveQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	last := queue.maxHeap.PeekFirst()
	if last == nil || !last.noBudget {
		return last
	}
	last = nil
	for _, item := range queue.orderedItems {
		if !item.noBudget && (last == nil || queue.cmp(last, item) < 0) {
			last = item
		}
	}
	return last
}

// Accounting for contracts remains in messageByteCount. These two queue views
// only decide DATA windows and DATA memory, which protocol does not consume.
func (self *receiveItem) MessageByteCount() ByteCount {
	if self.noBudget {
		return 0
	}
	return self.transferItem.MessageByteCount()
}
func (self *receiveItem) QueueByteCount() ByteCount {
	if self.noBudget {
		return 0
	}
	return self.transferItem.QueueByteCount()
}

// A metadata field cannot hide application bytes from receive admission.
func protocolOnlyPack(pack *protocol.Pack) bool {
	return pack != nil && (pack.ContractFrame == nil || pack.ContractFrame.MessageType == protocol.MessageType_TransferContract) && protocolOnlyFrames(pack.Frames)
}

// Raw protocol handoff slots stop bounding an owner once the worker takes it.
// Keep a separate count bound on ordered protocol hold metadata; DATA owners
// do not consume these slots, and current delivery and retained duplicates
// resolve before this admission check. A rendezvous has one active owner.
func (self *ReceiveSequence) canHoldProtocolItem() bool {
	queue := self.receiveQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	return queue.protocolItemCount < max(1, cap(self.protocolPacks))
}

// A local proof-only copy still represents its held DATA original. The
// existing root-owned repair identity lasts until this same owner returns.
// No new per-Packet flag or storage is needed; callers classify before locks.
func protocolOnlyReceivePack(pack *ReceivePack) bool {
	if pack == nil || !protocolOnlyPack(pack.Pack) {
		return false
	}
	if !pack.receiveMemoryOwned || pack.ingressSequence == nil {
		return true
	}
	return !pack.ingressSequence.receiveHeldHeadRepairCopy(pack)
}
