package connect

import (
	"reflect"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/v2026/protocol"
)

func (peer Peer) deliveryReceipt() *receiveDeliveryReceipt {
	receipt, _ := peer.delivery.(*receiveDeliveryReceipt)
	return receipt
}

// Reliable-provider retained accounting prepays the future packet owner as a
// distinct amount, in addition to all still-live receive roots and metadata.
// Admission occurs before out-of-order selective feedback can lease it.
func (self *ReceiveSequence) prepareReceiveDeliveryCredit(item *receiveItem) {
	if item.deliveryPrepared || !item.ack || self.receiveQueue == nil ||
		!self.receiveQueue.lifetimeBudget || !self.client.reliableProviderIngress.Load() ||
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

func (self *ReceiveSequence) flushDeliveryReceipts(items []*receiveItem, frames []*protocol.Frame, peer Peer) {
	if self.deliveryQueue == nil {
		self.deliveryQueue = newReceiveDeliveryQueue(self,
			self.receiveBufferSettings.SequenceBufferSize, self.receiveBufferSettings.ReceiveQueueMaxByteCount)
	}
	q := self.deliveryQueue
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
		if !item.ack {
			func() {
				defer item.messagePoolReturn()
				if len(appFrames) != 0 {
					item.receiveCallback(self.source, appFrames, peer)
				}
			}()
			continue
		}
		required := self.client.reliableProviderIngress.Load() && receiveDeliveryHasTcp(appFrames)
		receipt, accepted := q.append(item, required)
		if !accepted {
			// No ownership promise was made. Close before any later cumulative
			// head can step over this rejected item.
			item.messagePoolReturn()
			q.cancel()
			self.cancel()
			continue
		}
		func() {
			defer receipt.seal()
			itemPeer := peer
			if required {
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
	if !q.pending() {
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
