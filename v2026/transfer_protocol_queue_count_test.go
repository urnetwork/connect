// Count-only protocol admission follows actual queue membership through every
// heap removal path. The counter does not change payload or budget ownership.
package connect

import "testing"

func TestReceiveProtocolCountTracksQueueOwnershipTransitions(t *testing.T) {
	queue := newReceiveQueue(nil, 0)
	sequence := &ReceiveSequence{receiveQueue: queue, protocolPacks: make(chan *ReceivePack, 2)}
	makeItem := func(number uint64, protocol bool, bytes ByteCount) *receiveItem {
		return &receiveItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: number,
			messageByteCount: bytes, queueByteCount: bytes}, noBudget: protocol}
	}
	data := makeItem(10, false, 512)
	zeroData := makeItem(9, false, 0)
	first := makeItem(3, true, 0)
	second := makeItem(1, true, 0)
	third := makeItem(2, true, 0)
	check := func(wantItems, wantProtocols int, wantBytes ByteCount) {
		t.Helper()
		queue.stateLock.Lock()
		items, protocols, bytes := len(queue.orderedItems), queue.protocolItemCount, queue.byteCount
		queue.stateLock.Unlock()
		if items != wantItems || protocols != wantProtocols || bytes != wantBytes ||
			sequence.canHoldProtocolItem() != (wantProtocols < 2) {
			t.Fatalf("queue custody count changed: items=%d/%d protocols=%d/%d bytes=%d/%d", items, wantItems, protocols, wantProtocols, bytes, wantBytes)
		}
	}
	queue.Add(data)
	queue.Add(zeroData)
	check(2, 0, 512)
	queue.Add(first)
	check(3, 1, 512)
	queue.Add(second)
	check(4, 2, 512)
	if queue.RemoveByMessageId(first.messageId) != first {
		t.Fatal("message-ID removal changed the exact owner")
	}
	check(3, 1, 512)
	if queue.RemoveByMessageId(first.messageId) != nil || queue.RemoveBySequenceNumber(99) != nil {
		t.Fatal("absent owner removal fabricated custody")
	}
	check(3, 1, 512)
	queue.Add(third)
	check(4, 2, 512)
	if queue.RemoveBySequenceNumber(third.sequenceNumber) != third {
		t.Fatal("sequence-number removal changed the exact owner")
	}
	check(3, 1, 512)
	queue.Add(first)
	if queue.RemoveFirst() != second {
		t.Fatal("heap-first removal did not preserve receive order")
	}
	check(3, 1, 512)
	queue.Add(third)
	check(4, 2, 512)
	if returned := queue.Clear(); len(returned) != 4 {
		t.Fatal("clear lost a queued protocol or DATA owner")
	}
	check(0, 0, 0)
	queue.Add(first)
	check(1, 1, 0)
	if queue.RemoveFirst() != first || queue.RemoveFirst() != nil {
		t.Fatal("re-admission or empty removal changed owner identity")
	}
	check(0, 0, 0)
}

// Rendezvous admission has one active held protocol owner, while the receive
// DATA census and the sender's reliable-original lifetime remain independent.
func TestReceiveProtocolCountSeparatesRendezvousAndSendOwners(t *testing.T) {
	queue := newReceiveQueue(nil, 0)
	sequence := &ReceiveSequence{receiveQueue: queue, protocolPacks: make(chan *ReceivePack)}
	if !sequence.canHoldProtocolItem() {
		t.Fatal("empty rendezvous lost its one active receive owner")
	}
	item := &receiveItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}, noBudget: true}
	queue.Add(item)
	if sequence.canHoldProtocolItem() || queue.protocolItemCount != 1 {
		t.Fatal("rendezvous admitted a second held protocol owner")
	}
	queue.Clear()
	if !sequence.canHoldProtocolItem() || queue.protocolItemCount != 0 {
		t.Fatal("rendezvous count did not refund at actual queue disposal")
	}
	send := newTransferQueue[*sendItem](func(a, b *sendItem) int {
		if a.sequenceNumber < b.sequenceNumber {
			return -1
		}
		if b.sequenceNumber < a.sequenceNumber {
			return 1
		}
		return 0
	})
	send.Add(&sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 2}, noBudget: true})
	if send.protocolItemCount != 0 || send.Len() != 1 {
		t.Fatal("receive metadata counter reclassified serialized sender ownership")
	}
	send.RemoveFirst()
	if send.protocolItemCount != 0 {
		t.Fatal("sender removal changed the receive-only counter")
	}
}
