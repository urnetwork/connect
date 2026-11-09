// Merge controls for pre-dispatch receipt custody. Contract rejection and an
// earlier fatal callback must undo reservations before packet roots return.
package connect

import (
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// Runs the real worker's contract failure on either a fresh or held head,
// using both legacy accounting and exact retained admission.
//
// Exact retained admission decodes every Pack through the production scanner
// and paid decoder, as the client's receive loop does for that setting. Since
// 00d4f62b a held original commits its selective ACK only with repair escrow,
// and an explicit no-contract peer reserves just the inline owner, which
// cannot cover an original carrying a proof (receiveHeldHeadRepairLimit; see
// ReceiveHeadContractRepairByteCount). So under exact admission the held bad
// contract waits tentatively, without a lease, and must still release every
// owner when it reaches the head. Legacy accounting keeps its SACK lease.
func runReceiveDeliveryMergeContractFailure(t *testing.T, held bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	for _, retained := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(4), nil)
			budget := NewTransferMemoryBudget(kib(256))
			sequence := f.runningReceiveSequence(func(settings *ReceiveBufferSettings) {
				settings.SequenceBufferSize = 4
				settings.ReceiveQueueMinByteCount = 0
				settings.ReceiveQueueMaxByteCount = kib(64)
				settings.ReceiveQueueRetainedByteAccounting = retained
				if retained {
					settings.ReceiveQueueBudget = budget
				}
			})
			upload := func(number uint64, payload []byte, configure func(*protocol.Pack)) (*ReceivePack, Id) {
				t.Helper()
				pack, id := f.uploadPack(sequence, number, 101, payload)
				if configure != nil {
					configure(pack.Pack)
				}
				if !retained {
					return pack, id
				}
				defer pack.messagePoolReturn()
				owned := f.decodeOwnedPack(sequence, pack)
				if owned == nil {
					t.Fatal("paid decoder refused a Pack with free receive capacity")
				}
				return owned, id
			}
			number := uint64(0)
			if held {
				number = 1
			}
			pack, id := upload(number, []byte("rejected contract owner"), func(pack *protocol.Pack) {
				pack.ContractFrame = &protocol.Frame{MessageBytes: []byte{0x80}}
			})
			witness := MessagePoolShareReadOnly(pack.Pack.Frames[0].MessageBytes)
			defer func() { MessagePoolReturn(witness) }()
			if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
				pack.messagePoolReturn()
				t.Fatalf("contract fixture handoff: ok=%t err=%v", ok, err)
			}
			synctest.Wait()
			if held {
				item := sequence.receiveQueue.GetByMessageId(id)
				if item == nil || item.committed == retained || retained && item.memoryBudget != budget ||
					retained && (sequence.heldHeadRepair.eligibleCount != 0 || sequence.heldHeadRepair.reservedByteCount != 0) {
					t.Fatal("bad contract did not reach the real held-owner boundary")
				}
				head, _ := upload(0, []byte("valid predecessor"), nil)
				if ok, err := sequence.Pack(head, 0); !ok || err != nil {
					head.messagePoolReturn()
					t.Fatalf("predecessor handoff: ok=%t err=%v", ok, err)
				}
				synctest.Wait()
			}
			select {
			case <-sequence.exit:
			default:
				t.Fatal("contract failure did not join the receive worker")
			}
			q := sequence.deliveryQueue
			if sequence.ctx.Err() == nil || !sequence.rejectRetransmits ||
				sequence.nextSequenceNumber != number || q == nil {
				t.Fatal("fixture missed the terminal contract boundary before advancement")
			}
			if len(q.items) != 0 || len(q.admitted) != 0 || q.bytes != 0 || q.controlCount != 0 ||
				sequence.receiveQueue.Len() != 0 || budget.UsedByteCount() != 0 {
				t.Fatalf("retained=%t held=%t: contract failure stranded a receipt or reservation", retained, held)
			}
			if present, head := reliableIngressCumulativeHead(sequence); present && head.sequenceNumber >= number {
				t.Fatal("rejected contract acquired cumulative delivery evidence")
			}
			if f.client.Ctx().Err() != nil || f.provider.ctx.Err() != nil {
				t.Fatal("one contract failure closed the shared client/provider")
			}
			// The witness must be the only remaining pool owner after exit.
			probe := witness
			witness = nil
			if !MessagePoolReturn(probe) {
				t.Fatal("contract failure left the packet rooted outside its witness")
			}
		})
	}
}

// A fresh legacy admission must not leave an unsealed receipt after rejection;
// exact preflight must release its reservation on the same terminal path.
func TestReceiveDeliveryMergeContractFailureReleasesAdmission(t *testing.T) {
	runReceiveDeliveryMergeContractFailure(t, false)
}

// A held SACK owner can fail only when its contract reaches the head. That
// failure must release the exact held/admitted owner without a cumulative ACK.
func TestReceiveDeliveryMergeHeldContractFailureReleasesAdmission(t *testing.T) {
	runReceiveDeliveryMergeContractFailure(t, true)
}

// An earlier fatal receipt must suppress a later callback and undo its actual
// pre-admission, not merely return roots while leaving a ledger pointer to them.
func TestReceiveDeliveryMergeFatalBatchReleasesAdmittedReceipt(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, ctx := testReceiveDeliveryLedger(t, 4)
	q.maxBytes = kib(64)
	q.sequence.client = &Client{}
	q.sequence.client.reliableProviderIngress.Store(true)
	q.sequence.deliveryQueue = q
	first, later := testReceiveDeliveryItem(0), testReceiveDeliveryItem(1)
	defer func() {
		for _, item := range []*receiveItem{first, later} {
			q.discardAdmission(item)
			if item.frames != nil {
				item.messagePoolReturn()
			}
		}
	}()
	MessagePoolReturn(first.frames[0].MessageBytes)
	first.frames = nil
	packet := MessagePoolCopy(receiptAdmissionPacket(41000, tcpFlagAck, 101, []byte("fatal admitted claim")))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil {
		MessagePoolReturn(packet)
		t.Fatal(err)
	}
	first.frames = []*protocol.Frame{frame}
	first.receiveCallback = func(_ TransferPath, _ []*protocol.Frame, peer Peer) {
		claim := peer.deliveryReceipt().hold()
		if claim == nil {
			t.Fatal("fatal callback lacked its admitted claim")
		}
		claim.complete(false)
	}
	called := false
	later.receiveCallback = func(TransferPath, []*protocol.Frame, Peer) { called = true }
	if !q.admit(first) || !q.admit(later) || len(q.admitted) != 2 {
		t.Fatal("fixture did not reserve both real receipt owners")
	}
	q.sequence.flushDeliveryReceipts([]*receiveItem{first, later}, append([]*protocol.Frame{frame}, later.frames...), Peer{})
	if ctx.Err() == nil || called || first.frames != nil || later.frames != nil ||
		len(q.admitted) != 0 || q.pending() || q.bytes != 0 || q.sequence.ackWindow.Pending() {
		t.Fatal("fatal batch retained a pre-admitted owner, dispatched later data, or emitted an ACK")
	}
}

// A head-reset duplicate can pass exact preflight before the original held
// owner wins. The refused copy must release only its new reservation, not the
// original's SACK lease or the hold's promised predecessor credit.
//
// Since 00d4f62b a selective lease needs repair escrow that only a paid decoded
// owner can fund, so every copy enters through the production scanner and paid
// decoder, as in the client's receive loop. A copy's new reservation is then
// its own decode claim, taken before receive; it must be exactly what the
// refused copy returns.
func TestReceiveDeliveryMergeHeadDuplicateReturnsPreflightCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(4), nil)
		budget := NewTransferMemoryBudget(kib(256))
		settings := DefaultReceiveBufferSettings()
		settings.ReceiveQueueBudget = budget
		settings.ReceiveQueueMinByteCount = 0
		settings.ReceiveQueueMaxByteCount = kib(64)
		settings.ReceiveQueueRetainedByteAccounting = true
		sequence := NewReceiveSequence(f.ctx, f.client, f.source, NewId(), sequenceTlsRoleServer, false, settings)
		sequence.peerAudit = NewSequencePeerAudit(f.client, f.source, settings.MaxPeerAuditDuration)
		f.client.ContractManager().AddNoContractPeer(f.source.SourceId)
		defer func() {
			sequence.Close()
			if sequence.deliveryQueue != nil {
				sequence.deliveryQueue.cancel()
			}
			for _, item := range sequence.receiveQueue.Clear() {
				item.messagePoolReturn()
			}
			sequence.releaseHoldPromise()
			sequence.peerAudit.Complete()
			if budget.UsedByteCount() != 0 {
				t.Error("duplicate cleanup retained memory without a packet owner")
			}
		}()
		owned := func(number uint64, configure func(*protocol.Pack)) *ReceivePack {
			t.Helper()
			input, _ := f.uploadPack(sequence, number, 101, []byte("original held bytes"))
			defer input.messagePoolReturn()
			if configure != nil {
				configure(input.Pack)
			}
			pack := f.decodeOwnedPack(sequence, input)
			if pack == nil {
				t.Fatal("paid decoder refused a copy with free receive capacity")
			}
			return pack
		}
		// The later committed item makes sequence 1 itself lie below the
		// promise boundary. A duplicate must not spend that shared promise.
		later := owned(2, nil)
		if accepted, err := sequence.receive(later); !accepted || err != nil {
			later.messagePoolReturn()
			t.Fatalf("later receive: accepted=%t err=%v", accepted, err)
		}
		original := owned(1, nil)
		id := RequireIdFromBytes(original.Pack.MessageId)
		if accepted, err := sequence.receive(original); !accepted || err != nil {
			original.messagePoolReturn()
			t.Fatalf("original receive: accepted=%t err=%v", accepted, err)
		}
		held := sequence.receiveQueue.GetByMessageId(id)
		if held == nil || !held.committed || held.memoryBudget != budget {
			t.Fatal("original did not acquire real exact held custody")
		}
		before, promised, heldCharge := budget.UsedByteCount(), sequence.holdPromise.reservedByteCount, held.queueByteCount
		if !sequence.holdPromised(1) || promised <= 0 {
			t.Fatal("duplicate does not lie inside an actually funded promise")
		}
		wantNext := uint64(0)
		// Cover an out-of-order duplicate, a sender head reset, and then a
		// duplicate of the now-current held head, without running its drain.
		for _, head := range []bool{false, true, false} {
			reserved, released := budget.reservedByteCount.Load(), budget.releasedByteCount.Load()
			duplicate := owned(1, func(pack *protocol.Pack) {
				pack.MessageId, pack.Head = id.Bytes(), head
			})
			claim := duplicate.decodedOwner.receiveItem.QueueByteCount()
			if claim <= 0 || budget.reservedByteCount.Load()-reserved != int64(claim) ||
				sequence.holdPromise.reservedByteCount != promised {
				duplicate.messagePoolReturn()
				t.Fatal("duplicate decode borrowed the predecessor promise instead of paying for itself")
			}
			accepted, err := sequence.receive(duplicate)
			if !accepted {
				duplicate.messagePoolReturn()
			}
			if head {
				wantNext = 1
			}
			if accepted || err != nil || sequence.nextSequenceNumber != wantNext ||
				sequence.receiveQueue.GetByMessageId(id) != held || held.memoryBudget != budget ||
				budget.UsedByteCount() != before || held.queueByteCount != heldCharge ||
				sequence.holdPromise.reservedByteCount != promised ||
				budget.reservedByteCount.Load()-reserved != int64(claim) || budget.releasedByteCount.Load()-released != int64(claim) {
				t.Fatal("duplicate changed the original owner, spent its predecessor promise, or reserved fresh credit")
			}
		}
		if present, _ := reliableIngressCumulativeHead(sequence); present || len(sequence.deliverItems) != 0 {
			t.Fatal("duplicate was cumulatively acknowledged or dispatched before the original")
		}
	})
}
