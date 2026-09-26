package connect

import (
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/urnetwork/connect/v2026/protocol"
)

// receiveDeliveryQueue is a private, bounded downstream-admission ledger.
// Receive order and application progress are separate: a later TCP control
// can secure its owner while an earlier data item still waits, but it cannot
// advance the cumulative Transfer ACK across that earlier item.
//
// No public ReceiveFunction contract is changed. A provider takes explicit
// claims during its inline dispatch and completes them after final admission.
// The ledger itself starts no workers and never waits for downstream I/O.
type receiveDeliveryQueue struct {
	mutex        sync.Mutex
	sequence     *ReceiveSequence
	items        []*receiveDeliveryReceipt
	maxCount     int
	maxBytes     ByteCount
	bytes        ByteCount
	closed       bool
	failedFrom   uint64
	operations   []*receiveDeliveryOperation
	wake         chan struct{}
	controlCount int
	controlBytes ByteCount
}

type receiveDeliveryReceipt struct {
	queue               *receiveDeliveryQueue
	item                *receiveItem
	ack                 sequenceAck
	bytes               ByteCount
	messageBytes        ByteCount
	pending             int
	required            bool
	claimed             bool
	sealed              bool
	secured             bool
	control             bool
	controlReserve      bool
	firstSequenceNumber uint64
}

// Two small control owners permit one applied update to compact with its
// successor while data is full. This is not permission for extra data: only
// bounded ACK/RST-only IP frames may use the separate count/byte allowance.
const receiveDeliveryControlMaxCount = 2
const receiveDeliveryControlMaxBytes ByteCount = 8 * 1024

// A claim belongs to one downstream packet/group owner. Repeated or racing
// cancellation/completion can release its parent receipt only once.
type receiveDeliveryClaim struct {
	receipt   *receiveDeliveryReceipt
	completed atomic.Bool
}

func newReceiveDeliveryQueue(sequence *ReceiveSequence, maxCount int, maxBytes ByteCount) *receiveDeliveryQueue {
	return &receiveDeliveryQueue{sequence: sequence, maxCount: max(1, maxCount), maxBytes: max(1, maxBytes), wake: make(chan struct{}, 1)}
}

func (q *receiveDeliveryQueue) append(item *receiveItem, required bool) (*receiveDeliveryReceipt, bool) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if q.sequence.receiveQueue != nil {
		q.sequence.prepareReceiveDeliveryCredit(item)
	}
	// Charge the receipt, its ledger pointer and worst-case one claim per
	// application frame as well as the pre-existing retained item envelope.
	metadata := ByteCount(unsafe.Sizeof(receiveDeliveryReceipt{})) + 8 +
		ByteCount(max(1, len(item.frames)))*ByteCount(unsafe.Sizeof(receiveDeliveryClaim{})+unsafe.Sizeof(receiveDeliveryOperation{})+8)
	bytes := addReceiveQueueByteCount(max(1, item.QueueByteCount()), (metadata+63)/64*64)
	control := receiveDeliveryIsSmallControl(item)
	if q.closed {
		return nil, false
	}
	// The small control partition is additional headroom at saturation, not
	// a cap of two ordinary ACKs per decoded batch. Healthy controls use the
	// same bounded regular ledger as data; only actual count/byte/admission
	// pressure selects the independently prepaid control fallback.
	regular := len(q.items)-q.controlCount < q.maxCount && bytes <= q.maxBytes-(q.bytes-q.controlBytes)
	if regular && q.sequence.receiveQueue != nil && q.sequence.receiveQueue.lifetimeBudget && item.memoryBudget == nil {
		regular = item.reserveMemory(q.sequence.receiveQueue.budget, item.QueueByteCount())
	}
	controlReserve := !regular
	if controlReserve && (!control || q.controlCount >= receiveDeliveryControlMaxCount || bytes > receiveDeliveryControlMaxBytes-q.controlBytes) {
		return nil, false
	}
	r := &receiveDeliveryReceipt{queue: q, item: item, bytes: bytes, messageBytes: item.messageByteCount,
		required: required, control: control, controlReserve: controlReserve, firstSequenceNumber: item.sequenceNumber, ack: sequenceAck{
			receivedAtNanos: item.receivedAtNanos, sequenceNumber: item.sequenceNumber,
			messageId: item.messageId, tag: item.tag, compactContractRecoverySupported: true,
			unwrapped: item.unwrapped, transportType: item.transportType,
		}}
	q.items = append(q.items, r)
	q.bytes += bytes
	if controlReserve {
		q.controlCount++
		q.controlBytes += bytes
	}
	return r, true
}

func receiveDeliveryIsSmallControl(item *receiveItem) bool {
	if len(item.frames) == 0 || len(item.frames) > 2 {
		return false
	}
	for _, frame := range item.frames {
		if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider || len(frame.MessageBytes) > 512 {
			return false
		}
		packet, err := ipPacketToProviderBytes(frame)
		if err != nil || !smallNatControlPacket(packet) {
			return false
		}
	}
	return true
}

func (q *receiveDeliveryQueue) removeChargeLocked(r *receiveDeliveryReceipt) {
	q.bytes -= r.bytes
	if r.controlReserve {
		q.controlCount--
		q.controlBytes -= r.bytes
	}
}

func (r *receiveDeliveryReceipt) hold() *receiveDeliveryClaim {
	q := r.queue
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if q.closed || r.sealed {
		return nil
	}
	r.claimed = true
	r.pending++
	return &receiveDeliveryClaim{receipt: r}
}

func (r *receiveDeliveryReceipt) seal() {
	q := r.queue
	q.mutex.Lock()
	if r.sealed {
		q.mutex.Unlock()
		return
	}
	r.sealed = true
	failed := r.required && !r.claimed
	if failed && !q.closed {
		q.closed = true
		q.failedFrom = r.ack.sequenceNumber
	}
	returns := q.advanceLocked(r)
	q.mutex.Unlock()
	returnReceiveDeliveryItems(returns)
	if failed {
		q.sequence.cancel()
	}
}

func (claim *receiveDeliveryClaim) complete(secured bool) {
	if claim == nil || claim.completed.Swap(true) {
		return
	}
	r, q := claim.receipt, claim.receipt.queue
	q.mutex.Lock()
	r.pending--
	if !secured && !q.closed {
		q.closed = true
		q.failedFrom = r.ack.sequenceNumber
	}
	returns := q.advanceLocked(r)
	q.mutex.Unlock()
	returnReceiveDeliveryItems(returns)
	if !secured {
		q.sequence.cancel()
	}
}

// The callback must return before a borrowed item can go back to the pool,
// even if its downstream owner completed synchronously during the callback.
func (q *receiveDeliveryQueue) advanceLocked(changed *receiveDeliveryReceipt) []*receiveItem {
	var returns []*receiveItem
	if changed.sealed && changed.pending == 0 && !q.closed {
		changed.secured = true
	}
	if q.closed {
		remaining := q.items[:0]
		for _, r := range q.items {
			if r.sealed && r.pending == 0 {
				q.removeChargeLocked(r)
				if r.item != nil {
					returns = append(returns, r.item)
					r.item = nil
				}
			} else {
				remaining = append(remaining, r)
			}
		}
		clear(q.items[len(remaining):])
		q.items = remaining
		if len(q.items) == 0 {
			q.items = nil
		}
		return returns
	}
	if changed.secured && len(q.items) > 0 && q.items[0] != changed {
		ack := changed.ack
		ack.selective = true
		q.sequence.ackWindow.Update(ack)
	}
	// Applied TCP controls no longer need packet roots. Compact only adjacent
	// secured control spans, never across a data item or an unresolved receipt.
	// Older compacted duplicate identities conservatively receive no feedback
	// until the cumulative gap closes; they must not use the past-head shortcut.
	for index := 1; index < len(q.items); {
		previous, current := q.items[index-1], q.items[index]
		if !previous.control || !current.control || !previous.secured || !current.secured ||
			previous.ack.sequenceNumber+1 != current.firstSequenceNumber {
			index++
			continue
		}
		current.firstSequenceNumber = previous.firstSequenceNumber
		current.messageBytes += previous.messageBytes
		q.removeChargeLocked(previous)
		if previous.item != nil {
			returns = append(returns, previous.item)
			previous.item = nil
		}
		q.sequence.ackWindow.ackLock.Lock()
		delete(q.sequence.ackWindow.selectiveAcks, previous.ack.messageId)
		q.sequence.ackWindow.ackLock.Unlock()
		copy(q.items[index-1:], q.items[index:])
		q.items[len(q.items)-1] = nil
		q.items = q.items[:len(q.items)-1]
	}
	for len(q.items) > 0 && q.items[0].secured {
		r := q.items[0]
		q.items[0] = nil
		q.items = q.items[1:]
		q.removeChargeLocked(r)
		q.sequence.ackWindow.UpdateDelivered(r.ack, r.messageBytes)
		if r.item != nil {
			returns = append(returns, r.item)
			r.item = nil
		}
	}
	return returns
}

// A pending/refused item is not a delivered past head. A secured later item
// may repeat only selective evidence until the cumulative prefix catches up.
func (q *receiveDeliveryQueue) duplicate(sequenceNumber uint64, messageID Id) (known, secured bool) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if q.closed && sequenceNumber >= q.failedFrom {
		return true, false
	}
	for _, r := range q.items {
		if r.firstSequenceNumber <= sequenceNumber && sequenceNumber <= r.ack.sequenceNumber {
			return true, r.ack.sequenceNumber == sequenceNumber && r.ack.messageId == messageID && r.secured
		}
	}
	return false, false
}

func (q *receiveDeliveryQueue) cancel() {
	q.mutex.Lock()
	if !q.closed {
		q.closed = true
		if len(q.items) > 0 {
			q.failedFrom = q.items[0].ack.sequenceNumber
		}
	}
	returns := q.advanceLocked(&receiveDeliveryReceipt{})
	operations := append([]*receiveDeliveryOperation(nil), q.operations...)
	q.mutex.Unlock()
	returnReceiveDeliveryItems(returns)
	for _, operation := range operations {
		operation.cancel()
	}
}

func returnReceiveDeliveryItems(items []*receiveItem) {
	for _, item := range items {
		item.messagePoolReturn()
	}
}
