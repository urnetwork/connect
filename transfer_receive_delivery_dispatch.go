package connect

import (
	"reflect"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

func (peer Peer) deliveryReceipt() *receiveDeliveryReceipt {
	receipt, _ := peer.delivery.(*receiveDeliveryReceipt)
	return receipt
}

// Reliable-provider retained accounting prepays the future packet owner as a
// distinct amount, in addition to all still-live receive roots and metadata.
// Classify TCP before provider publication; admission precedes any SACK lease.
func (self *ReceiveSequence) prepareReceiveDeliveryCredit(item *receiveItem) {
	if item.deliveryPrepared || !item.ack || self.receiveQueue == nil ||
		!self.receiveQueue.lifetimeBudget ||
		!receiveDeliveryHasTcp(item.frames) {
		return
	}
	item.deliveryPrepared = true
	metadata := ByteCount(unsafe.Sizeof(receiveDeliveryReceipt{})) + 64
	for _, frame := range item.frames {
		if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
			continue
		}
		packet, err := ipPacketToProviderBytes(frame)
		if err != nil || isIpFragmentPacket(packet) {
			continue
		}
		var path IpPath
		if _, err := parseIpPathWithPayloadBorrowed(packet, &path); err != nil || path.Protocol != IpProtocolTcp {
			continue
		}
		// Normalized legacy/control roots cannot exceed this rounded size;
		// raw data may retain a larger borrowed root, so charge the maximum.
		root := max(ByteCount(cap(packet)), retainedMessageCapacity(ByteCount(len(packet))))
		item.deliveryPrepaid += 320 + root + ByteCount(unsafe.Sizeof(TcpSendItem{})) + 128
		metadata += ByteCount(unsafe.Sizeof(receiveDeliveryOperation{})+unsafe.Sizeof(receiveDeliveryClaim{})+
			unsafe.Sizeof(providerReliablePacket{})) + 512
	}
	item.queueByteCount = addReceiveQueueByteCount(item.queueByteCount,
		item.deliveryPrepaid+(metadata+63)/64*64)
}

func receiveDeliveryHasTcp(frames []*protocol.Frame) bool {
	for _, frame := range frames {
		if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
			continue
		}
		packet, err := ipPacketToProviderBytes(frame)
		if err != nil || isIpFragmentPacket(packet) {
			continue
		}
		var path IpPath
		if _, err := parseIpPathWithPayloadBorrowed(packet, &path); err == nil && path.Protocol == IpProtocolTcp {
			return true
		}
	}
	return false
}

// Reports whether the ready head must stay in the receive hold because the
// legacy delivery ledger cannot take its receipt yet. Exact accounting already
// reserved its owner in preflight. A full ledger is backpressure,
// not a failure: the held head keeps the cumulative ACK, and with it the
// sender's byte window, where it is until a drain wakes the sequence.
func (self *ReceiveSequence) holdHead(item *receiveItem) bool {
	if !item.ack {
		return false
	}
	if self.receiveQueue != nil && self.receiveQueue.lifetimeBudget {
		// Mobile/hierarchical exact accounting has already reserved this
		// owner in prepareReceiveDelivery, after flushing its prior batch.
		return false
	}
	if self.deliveryQueue == nil {
		if !receiveDeliveryHasTcp(item.frames) {
			return false
		}
		// The buffered heads need no receipts. Deliver them first, so every
		// head the ledger ever owns was admitted before its delivery.
		self.flushDeliver()
		self.deliveryQueue = newReceiveDeliveryQueue(self,
			self.receiveBufferSettings.SequenceBufferSize, self.receiveBufferSettings.ReceiveQueueMaxByteCount)
	}
	return !self.deliveryQueue.admit(item)
}

// The ledger's drain edge, or nil (never ready) before a ledger exists. A
// sequence holding its head must wake for it even after the ledger empties.
func (self *ReceiveSequence) deliveryWake() <-chan struct{} {
	if self.deliveryQueue == nil {
		return nil
	}
	return self.deliveryQueue.wake
}

func (self *ReceiveSequence) needsDeliveryReceipts(items []*receiveItem, frames []*protocol.Frame) bool {
	if self.deliveryQueue != nil {
		return true
	}
	if !self.client.reliableProviderIngress.Load() || !receiveDeliveryHasTcp(frames) {
		return false
	}
	for _, item := range items {
		if item.ack {
			return true
		}
	}
	return false
}

// Runs before exact-owner head advancement, any sender-directed head reset,
// or hold removal. Exact accounting prepays the downstream owner and refuses
// fresh pressure without an ACK. Legacy accounting instead charges pending
// batch receipts in holdHead and may retain a fresh head in its bounded hold.
// An existing held item keeps its owner and SACK lease in either mode.
func (self *ReceiveSequence) prepareReceiveDelivery(item *receiveItem) bool {
	if !item.ack || self.deliveryQueue == nil && !receiveDeliveryHasTcp(item.frames) {
		return true
	}
	if self.deliveryQueue == nil || self.receiveQueue != nil && self.receiveQueue.lifetimeBudget {
		self.flushDeliver()
	}
	if self.deliveryQueue == nil {
		self.deliveryQueue = newReceiveDeliveryQueue(self,
			self.receiveBufferSettings.SequenceBufferSize, self.receiveBufferSettings.ReceiveQueueMaxByteCount)
	}
	return self.deliveryQueue.prepare(item)
}

// A contract failure precedes dispatch, so the worker must undo a legacy
// receipt reservation before its existing caller returns the packet roots.
func (self *ReceiveSequence) discardReceiveDeliveryAdmission(item *receiveItem) {
	if self.deliveryQueue != nil {
		self.deliveryQueue.discardAdmission(item)
	}
}

// A TCP owner must fit before it can acquire a selective lease. Ordinary
// callbacks retain their one-oversize-owner rule once earlier receipts drain.
func (self *ReceiveSequence) receiveDeliveryFits(item *receiveItem) bool {
	if !item.ack || !receiveDeliveryHasTcp(item.frames) {
		return true
	}
	bytes := receiveDeliveryByteCount(item)
	return bytes <= max(1, self.receiveBufferSettings.ReceiveQueueMaxByteCount) ||
		receiveDeliveryIsSmallControl(item) && bytes <= receiveDeliveryControlMaxBytes
}

func (self *ReceiveSequence) flushDeliveryReceipts(items []*receiveItem, frames []*protocol.Frame, peer Peer) {
	if self.deliveryQueue == nil {
		self.deliveryQueue = newReceiveDeliveryQueue(self,
			self.receiveBufferSettings.SequenceBufferSize, self.receiveBufferSettings.ReceiveQueueMaxByteCount)
	}
	q := self.deliveryQueue
	nextItem := 0
	defer func() {
		// An internal dispatcher panic must not strand later pre-admitted
		// receipts or their roots. The current callback has its own defer.
		for _, item := range items[nextItem:] {
			q.discardAdmission(item)
			item.messagePoolReturn()
		}
	}()
	// Boundaries describe application frames after encrypted-control filtering.
	// The fallback is for callers constructing unfiltered items directly.
	offset := 0
	for _, item := range items {
		count := item.deliverFrameCount
		if !item.deliverFrameCountSet {
			count = len(item.frames)
		}
		if count < 0 || count > len(frames)-offset {
			panic("invalid receive delivery frame boundary")
		}
		appFrames := frames[offset : offset+count]
		offset += count
		nextItem++
		q.mutex.Lock()
		closed := q.closed
		q.mutex.Unlock()
		if closed {
			// A prior fatal rejection in this batch revokes later dispatch,
			// including unacknowledged items, without losing their roots.
			q.discardAdmission(item)
			item.messagePoolReturn()
			continue
		}
		if !item.ack {
			func() {
				defer item.messagePoolReturn()
				if len(appFrames) != 0 {
					item.receiveCallback(self.source, appFrames, peer)
				}
			}()
			continue
		}
		tcp := receiveDeliveryHasTcp(appFrames)
		required := self.client.reliableProviderIngress.Load() && tcp
		receipt, accepted := q.receipt(item, required)
		if !accepted {
			// Only a head delivered without admission reaches this. No
			// ownership promise was made; close before any later cumulative
			// head can step over this rejected item.
			item.messagePoolReturn()
			q.cancel()
			self.cancel()
			continue
		}
		func() {
			defer receipt.seal()
			itemPeer := peer
			// A provider callback can be published before its required-owner
			// flag. Immutable TCP classification supplies its optional claim
			// now; a real claim still delays ACK until secured.
			if tcp {
				itemPeer.delivery = receipt
			}
			if len(appFrames) != 0 {
				item.receiveCallback(self.source, appFrames, itemPeer)
			}
		}()
	}
	q.pump()
}

func (q *receiveDeliveryQueue) pending() bool {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	return len(q.items) != 0
}

func (self *ReceiveSequence) sendDeliveredDuplicateAck(number uint64, id Id, tag sequenceTag, unwrapped bool, transport TransportType) {
	for _, item := range self.deliverItems {
		if item.sequenceNumber == number {
			// Decoded/batched is not yet delivered. A duplicate can arrive
			// in the same drain burst before the receipt ledger is created.
			return
		}
	}
	selective := false
	if self.deliveryQueue != nil {
		known, secured := self.deliveryQueue.duplicate(number, id)
		if known && !secured {
			return
		}
		selective = known
	}
	self.sendAck(number, id, selective, tag, unwrapped, transport)
}

// Only the pending-owner slow path builds a dynamic select. Ordinary Pack
// drains and waits retain their static selects. No polling or waiter worker.
func (self *ReceiveSequence) waitDelivery(timeout <-chan time.Time) (*ReceivePack, bool, bool) {
	q := self.deliveryQueue
	wakes := q.pump()
	var blocked *receiveItem
	if self.receiveQueue != nil {
		blocked = self.receiveQueue.PeekFirst()
		if blocked != nil && blocked.sequenceNumber != self.nextSequenceNumber {
			blocked = nil
		}
	}
	if blocked != nil {
		// Subscribe before the exact reservation attempt, even with no
		// pending receipt. Another sequence may own the shared bytes.
		if budget := self.receiveQueue.budget; budget != nil {
			wakes = append(wakes, budget.CapacityNotify())
		}
		if self.prepareReceiveDelivery(blocked) {
			return nil, true, false
		}
	}
	if blocked == nil && !q.pending() {
		return nil, true, false
	}
	cases := make([]reflect.SelectCase, 0, 4+len(wakes))
	for _, channel := range []any{self.ctx.Done(), self.packs, timeout, q.wake} {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(channel)})
	}
	for _, wake := range wakes {
		if wake != nil {
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(wake)})
		}
	}
	chosen, value, ok := reflect.Select(cases)
	if chosen == 1 {
		if !ok {
			return nil, false, true
		}
		pack := value.Interface().(*ReceivePack)
		self.releasePackQueue(pack)
		return pack, true, false
	}
	return nil, true, chosen == 2
}
