package connect

import (
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

// receiveDeliveryQueue is a private, bounded downstream-admission ledger.
// Receive order and application progress are separate: a later TCP control
// can secure its owner while an earlier data item still waits, but it cannot
// advance the cumulative Transfer ACK across that earlier item.
//
// No public ReceiveFunction contract is changed. A provider takes explicit
// claims during its inline dispatch and completes them after final admission.
// The ledger itself starts no workers and never waits for downstream I/O.
//
// Legacy accounting admits each head's receipt before batching its delivery;
// a full ledger leaves the head in the bounded receive hold. Exact retained
// accounting instead reserves the live item and downstream credit before any
// head advance, refusing a fresh Pack without ACK when that admission fails.
// Both modes retain an already-held SACK owner until downstream admission.
type receiveDeliveryQueue struct {
	mutex    sync.Mutex
	sequence *ReceiveSequence
	items    []*receiveDeliveryReceipt
	// Receipts admitted with their heads, in delivery order, whose batch
	// callback has not yet run. The flush takes each with its own item.
	admitted     []*receiveDeliveryReceipt
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

// Includes the retained packet owner and bounded receipt/claim metadata.
func receiveDeliveryByteCount(item *receiveItem) ByteCount {
	// The receipt can be in both the ordered ledger and the admitted batch.
	metadata := ByteCount(unsafe.Sizeof(receiveDeliveryReceipt{})) + 16 +
		ByteCount(max(1, len(item.frames)))*ByteCount(unsafe.Sizeof(receiveDeliveryClaim{})+unsafe.Sizeof(receiveDeliveryOperation{})+8)
	return addReceiveQueueByteCount(max(1, item.QueueByteCount()), (metadata+63)/64*64)
}

// Appends a directly dispatched item, using the same limits as preflight.
func (q *receiveDeliveryQueue) append(item *receiveItem, required bool) (*receiveDeliveryReceipt, bool) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	return q.appendLocked(item, required, false)
}

// Admits the receipt of a head before the head is delivered. A refusal
// leaves nothing to undo: the caller keeps the head in the receive hold, and
// a drain of this ledger wakes the sequence to try again. An empty ledger
// takes any one owner its memory budget admits, as the hold does, so no
// head can wait for a drain that can never come.
func (q *receiveDeliveryQueue) admit(item *receiveItem) bool {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	r, ok := q.appendLocked(item, false, true)
	if ok {
		q.admitted = append(q.admitted, r)
	}
	return ok
}

// The receipt admitted with item's head. A head dispatched without that
// admission (a direct dispatch, or reliable ingress enabled while its batch
// was pending) is appended now and can still be refused.
func (q *receiveDeliveryQueue) receipt(item *receiveItem, required bool) (*receiveDeliveryReceipt, bool) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if 0 < len(q.admitted) && q.admitted[0].item == item {
		r := q.admitted[0]
		q.admitted[0] = nil
		q.admitted = q.admitted[1:]
		r.required = required
		return r, true
	}
	return q.appendLocked(item, required, false)
}

// Exact accounting flushes the previous batch before this actual reservation;
// only the receive worker can append. Legacy accounting has already charged
// the pending batch's receipts and needs no forced flush here.
func (q *receiveDeliveryQueue) prepare(item *receiveItem) bool {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	admitOne := q.sequence.receiveQueue != nil && !q.sequence.receiveQueue.lifetimeBudget
	_, _, _, accepted := q.prepareWithLock(item, admitOne)
	return accepted
}

// Shares the exact limits and reservation with the eventual ledger append.
func (q *receiveDeliveryQueue) prepareWithLock(item *receiveItem, admitOne bool) (ByteCount, bool, bool, bool) {
	if q.sequence.receiveQueue != nil {
		q.sequence.prepareReceiveDeliveryCredit(item)
	}
	bytes := receiveDeliveryByteCount(item)
	control := receiveDeliveryIsSmallControl(item)
	if q.closed {
		return 0, false, false, false
	}
	// The small control partition is additional headroom at saturation, not
	// a cap of two ordinary ACKs per decoded batch. Healthy controls use the
	// same bounded regular ledger as data; only actual count/byte/admission
	// pressure selects the independently prepaid control fallback.
	// An ordinary callback cannot retain a provider claim. Preserve its
	// existing one-oversize-owner progress rule, only with an empty ledger.
	synchronousOversize := len(q.items) == 0 && !receiveDeliveryHasTcp(item.frames)
	regular := len(q.items)-q.controlCount < q.maxCount &&
		(bytes <= q.maxBytes-(q.bytes-q.controlBytes) || synchronousOversize || admitOne && len(q.items) == 0)
	if regular && q.sequence.receiveQueue != nil && q.sequence.receiveQueue.lifetimeBudget && item.memoryBudget == nil {
		// A selectively held successor already reserved its missing heads.
		// Move that exact credit into the arriving owner before requesting
		// fresh bytes, just as admission into the out-of-order hold does.
		regular = q.sequence.reserveHeldItem(item, q.sequence.holdPromised(item.sequenceNumber))
	}
	controlReserve := !regular
	if controlReserve && (!control || q.controlCount >= receiveDeliveryControlMaxCount || bytes > receiveDeliveryControlMaxBytes-q.controlBytes) {
		if ackLineageTraceEnabled {
			reason := "memory"
			if len(q.items)-q.controlCount >= q.maxCount {
				reason = "count"
			} else if bytes > q.maxBytes-(q.bytes-q.controlBytes) {
				reason = "bytes"
			}
			q.traceReceiptStateLocked(&receiveDeliveryReceipt{ack: sequenceAck{messageId: item.messageId, sequenceNumber: item.sequenceNumber}, messageBytes: item.messageByteCount}, "delivery_receipt_refused", false, reason)
		}
		return 0, false, false, false
	}
	return bytes, control, controlReserve, true
}

// Creates one receipt only after all count, byte and retained-owner checks.
func (q *receiveDeliveryQueue) appendLocked(item *receiveItem, required bool, admitOne bool) (*receiveDeliveryReceipt, bool) {
	bytes, control, controlReserve, accepted := q.prepareWithLock(item, admitOne)
	if !accepted {
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
	if ackLineageTraceEnabled {
		q.traceReceiptLocked(r, "delivery_receipt_admitted", true)
	}
	return r, true
}

// Undoes only a not-yet-dispatched admission. The worker still owns item's
// roots and returns them on contract rejection or an aborted batch. Removing
// the receipt first prevents a later cancel/flush from observing pooled data.
func (q *receiveDeliveryQueue) discardAdmission(item *receiveItem) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	for index, r := range q.admitted {
		if r.item != item {
			continue
		}
		copy(q.admitted[index:], q.admitted[index+1:])
		q.admitted[len(q.admitted)-1] = nil
		q.admitted = q.admitted[:len(q.admitted)-1]
		for receiptIndex, receipt := range q.items {
			if receipt == r {
				copy(q.items[receiptIndex:], q.items[receiptIndex+1:])
				q.items[len(q.items)-1] = nil
				q.items = q.items[:len(q.items)-1]
				break
			}
		}
		q.removeChargeLocked(r)
		r.item = nil
		return
	}
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
	// Bare claim completion and seal may free capacity outside q.pump.
	q.notify()
}

// After the callback returns, only an owner that still holds a claim can
// claim again: a deferred decode hands its frame to the NAT owner it creates.
func (r *receiveDeliveryReceipt) hold() *receiveDeliveryClaim {
	q := r.queue
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if q.closed || r.sealed && r.pending == 0 {
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
		if ackLineageTraceEnabled {
			q.traceReceiptStateLocked(r, "delivery_receipt_rejected", false, "unclaimed")
		}
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
		if ackLineageTraceEnabled {
			q.traceReceiptStateLocked(r, "delivery_receipt_rejected", false, "claim")
		}
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
		if ackLineageTraceEnabled && !changed.secured {
			q.traceReceiptLocked(changed, "delivery_receipt_secured", true)
		}
		changed.secured = true
	}
	if ackLineageTraceEnabled && changed.sealed && changed.pending > 0 {
		q.traceReceiptLocked(changed, "delivery_receipt_wait", false)
	}
	if q.closed {
		if ackLineageTraceEnabled && changed.queue == q {
			q.traceReceiptLocked(changed, "delivery_receipt_closed", false)
		}
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
	// One completion can secure a run of receipts at once, when an earlier
	// owner finishes after later ones. The ACK worker drains concurrently, so
	// publish the run at once, as flushDeliver publishes a delivered batch.
	run := q.items
	secured := 0
	for len(q.items) > 0 && q.items[0].secured {
		r := q.items[0]
		q.items = q.items[1:]
		secured++
		q.removeChargeLocked(r)
		if ackLineageTraceEnabled {
			q.traceReceiptLocked(r, "delivery_receipt_prefix", true)
		}
		if r.item != nil {
			returns = append(returns, r.item)
			r.item = nil
		}
	}
	if 0 < secured {
		q.sequence.ackWindow.UpdateDeliveredPrefix(func() {
			for _, r := range run[:secured] {
				q.sequence.ackWindow.updateDeliveredWithLock(r.ack, r.messageBytes)
			}
		})
		clear(run[:secured])
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
		if ackLineageTraceEnabled && len(q.items) > 0 {
			q.traceReceiptStateLocked(q.items[0], "delivery_queue_canceled", false, "sequence")
		}
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
