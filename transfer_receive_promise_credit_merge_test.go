// Exact receive-head admission composes with the held successor's promise;
// downstream credit moves independently of retiring that predecessor promise.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// A committed successor has already paid for its missing data head. Filling
// the common root afterwards must not force a second reservation or a retry.
func TestReceiveDeliveryPromisedDataHeadUsesExistingCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := NewTransferMemoryBudget(mib(1))
		budget := NewTransferMemoryBudgetWithParent(kib(64), root)
		f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(4), nil)
		sequence := f.runningReceiveSequence(reliableIngressBudgetSettings(budget))
		payload := bytes.Repeat([]byte{0x6d}, 1200)
		later, laterId := f.uploadPack(sequence, 1, 1301, payload)
		if ok, err := sequence.Pack(later, 0); !ok || err != nil {
			later.messagePoolReturn()
			t.Fatalf("later Pack handoff: ok=%t err=%v", ok, err)
		}
		synctest.Wait()
		held := sequence.receiveQueue.GetByMessageId(laterId)
		if held == nil || !held.committed || held.memoryBudget != budget ||
			sequence.holdPromise.reservedByteCount != held.queueByteCount {
			t.Fatal("successor did not prepay its missing data head")
		}
		fill := root.Available()
		if !root.TryReserve(fill) {
			t.Fatal("could not saturate the common root after the promise")
		}
		defer root.Release(fill)
		reserved, released := root.reservedByteCount.Load(), root.releasedByteCount.Load()
		head, _ := f.uploadPack(sequence, 0, 101, payload)
		if ok, err := sequence.Pack(head, 0); !ok || err != nil {
			head.messagePoolReturn()
			t.Fatalf("head Pack handoff: ok=%t err=%v", ok, err)
		}
		synctest.Wait()
		if sequence.nextSequenceNumber != 2 || sequence.receiveQueue.Len() != 0 ||
			sequence.holdPromise.reservedByteCount != 0 || sequence.deliveryQueue == nil ||
			len(sequence.deliveryQueue.items) != 2 {
			t.Fatal("prepaid data head was refused at the full root instead of taking its promised owner")
		}
		if root.UsedByteCount() != root.TotalByteCount() || root.reservedByteCount.Load() != reserved ||
			root.releasedByteCount.Load() != released {
			t.Fatal("head admission released or re-reserved promised memory")
		}
		if present, _ := reliableIngressCumulativeHead(sequence); present {
			t.Fatal("data was cumulatively acknowledged before the full TCP queue admitted it")
		}
		// The occupied SYN is the real final-queue barrier. Release it, then
		// each data owner; no new Pack or synthetic ACK drives progress.
		(<-f.tcp.sendItems).release()
		for _, number := range []uint32{101, 1301} {
			synctest.Wait()
			select {
			case item := <-f.tcp.sendItems:
				if item.tcp.seq != number || !bytes.Equal(item.tcp.payload, payload) {
					t.Error("promised data arrived at the final TCP queue out of order")
				}
				item.release()
			default:
				t.Fatal("promised data did not reach final TCP admission after capacity release")
			}
		}
		synctest.Wait()
		if present, ack := reliableIngressCumulativeHead(sequence); !present || ack.messageId != laterId || budget.UsedByteCount() != 0 {
			t.Fatal("final admission did not settle the exact receive owners and cumulative prefix")
		}
	})
}

// This isolated ownership boundary has no predecessor retirement in its
// observation interval. A refused target child changes nothing; a successful
// move changes only the two child ledgers, never the saturated common root.
func TestReceiveDeliveryPacketCreditMoveKeepsCommonRootCounters(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 2)
	q.maxBytes = kib(64)
	root := NewTransferMemoryBudget(kib(64))
	source := NewTransferMemoryBudgetWithParent(kib(64), root)
	target := NewTransferMemoryBudgetWithParent(0, root)
	q.sequence.receiveQueue = newReceiveQueue(source, 0)
	q.sequence.receiveQueue.setLifetimeBudget()
	packet := MessagePoolCopy(receiptAdmissionPacket(41000, tcpFlagAck, 101, bytes.Repeat([]byte{0x6d}, 1200)))
	item := &receiveItem{transferItem: transferItem{messageId: NewId(), queueByteCount: MessagePoolRootByteCount(packet)}, ack: true,
		frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}}}
	receipt, accepted := q.append(item, true)
	if !accepted {
		item.messagePoolReturn()
		t.Fatal("fixture could not reserve its actual receive receipt")
	}
	claim := receipt.hold()
	if claim == nil {
		receipt.seal()
		t.Fatal("receipt did not expose its packet ownership claim")
	}
	receipt.seal()
	fill := root.Available()
	if !root.TryReserve(fill) {
		claim.complete(false)
		t.Fatal("fixture could not saturate the common root")
	}
	var moved natMemoryReservation
	defer func() {
		moved.release()
		claim.complete(false)
		root.Release(fill)
		for _, budget := range []*TransferMemoryBudget{root, source, target} {
			if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
				t.Error("isolated credit handoff left an unbalanced owner")
			}
		}
	}()
	charge, prepaid := item.queueByteCount, item.deliveryPrepaid
	rootBytes := MessagePoolRootByteCount(packet)
	transferBytes := ByteCount(320) + natPacketMemoryByteCount(packet)
	reserved, released := root.reservedByteCount.Load(), root.releasedByteCount.Load()
	if memory, ok := receipt.takePacketCredit(target, transferBytes); ok {
		memory.release()
		t.Fatal("zero-capacity target admitted a prepaid packet")
	}
	if item.queueByteCount != charge || item.deliveryPrepaid != prepaid || source.UsedByteCount() != charge ||
		target.UsedByteCount() != 0 || root.UsedByteCount() != root.TotalByteCount() ||
		root.reservedByteCount.Load() != reserved || root.releasedByteCount.Load() != released {
		t.Fatal("target refusal changed a source owner or common-root counter")
	}
	target.SetTotalByteCount(transferBytes)
	moved, accepted = receipt.takePacketCredit(target, transferBytes)
	if !accepted || moved.budget != target || moved.bytes != transferBytes ||
		item.queueByteCount != charge-transferBytes || item.deliveryPrepaid != prepaid-transferBytes ||
		item.queueByteCount < rootBytes || source.UsedByteCount() != item.queueByteCount || target.UsedByteCount() != transferBytes ||
		root.UsedByteCount() != root.TotalByteCount() || root.reservedByteCount.Load() != reserved || root.releasedByteCount.Load() != released {
		t.Fatal("packet-credit move changed the common root or released a still-live receive root")
	}
}
