// Exact receive fixtures enter through the production scanner and paid decoder
// before their isolated sequence worker handles ordering and final TCP custody.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
)

// Borrows payload; hands one fully decoded original to the real sequence worker.
// Legacy direct fixtures keep their separate admission/accounting premise.
func (self *reliableTcpIngressFixture) packOwned(sequence *ReceiveSequence, number uint64, payload []byte) Id {
	self.t.Helper()
	return self.packOwnedTcp(sequence, number, 101, payload)
}

// The same paid handoff with the inner TCP sequence number chosen by the caller.
func (self *reliableTcpIngressFixture) packOwnedTcp(sequence *ReceiveSequence, number uint64, tcpSequenceNumber uint32, payload []byte) Id {
	self.t.Helper()
	if !sequence.receiveBufferSettings.ownsDecodedReceiveMemory() {
		self.t.Fatal("owned provider fixture requires exact receive accounting")
	}
	input, id := self.uploadPack(sequence, number, tcpSequenceNumber, payload)
	pack := func() *ReceivePack {
		defer input.messagePoolReturn()
		return self.decodeOwnedPack(sequence, input)
	}()
	if pack == nil {
		self.t.Fatal("owned provider fixture could not fund its encoded original")
	}
	if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
		pack.messagePoolReturn()
		self.t.Fatalf("owned provider fixture handoff failed: accepted=%t err=%v", ok, err)
	}
	return id
}

// Borrows the constructed Pack and returns a separately owned decoded carrier.
// The fixture runs its sequence outside the Client's sequence manager, so the
// decoder uses a private lookup containing that same live sequence and budget.
func (self *reliableTcpIngressFixture) decodeOwnedPack(sequence *ReceiveSequence, input *ReceivePack) *ReceivePack {
	self.t.Helper()
	pack := input.Pack
	wire := marshalSendPackTransferFrame(&sendPackFrame{path: self.source,
		messageId: RequireIdFromBytes(pack.MessageId), sequenceId: sequence.sequenceId,
		sequenceNumber: pack.SequenceNumber, head: pack.Head, nack: pack.Nack,
		frames: pack.Frames, contractFrame: pack.ContractFrame, contractAhead: pack.ContractAhead,
		forceStream: pack.ForceStream, companionContract: pack.CompanionContract, logicalLane: pack.LogicalLane})
	defer func() { MessagePoolReturn(wire) }()
	settings := *self.client.settings
	settings.ReceiveBufferSettings = sequence.receiveBufferSettings
	key := receiveSequenceId{Source: self.source.SourceMask(), SequenceId: sequence.sequenceId,
		EncryptionRole: sequenceTlsRoleServer, LogicalLane: pack.LogicalLane}
	decoder := &Client{settings: &settings, receiveBuffer: &ReceiveBuffer{
		receiveBufferSettings: settings.ReceiveBufferSettings,
		receiveSequences:      map[receiveSequenceId]*ReceiveSequence{key: sequence},
	}}
	decoder.reliableProviderIngress.Store(self.client.reliableProviderIngress.Load())
	decoded := &decodedTransferFrame{}
	defer decoded.release()
	if ok, pressure := decoder.unmarshalReceiveMemoryFrame(wire, decoded, true, self.source, false, sequenceTlsRoleServer, false); !ok || pressure {
		if !pressure {
			self.t.Fatal("valid provider fixture wire failed owned decoding")
		}
		return nil
	}
	owner := decoded.detachPackOwner()
	if owner == nil {
		self.t.Fatal("exact provider fixture did not decode a physical owner")
	}
	output := &owner.receivePack
	root := owner.receiveItem.memoryBudget.admissionRoot()
	root.admissionLock.Lock()
	output.Source, output.SequenceId = self.source, sequence.sequenceId
	output.Ctx, output.Unwrapped, output.EncryptionRole = self.ctx, true, sequenceTlsRoleServer
	output.ReceiveCallback, output.MessageByteCount = self.client.receiveCallback, MessageByteCount(output.Pack.Frames)
	output.TransferFrameBytes, output.TransportType = wire, TransportTypeH1
	output.receivePublishedReliable = !output.Pack.Nack
	output.receiveConsumerData = receiveHeadRecoveryPack(output.Pack)
	actual := MessagePoolRootByteCount(wire) + owner.metadataQueueByteCount()
	for _, frame := range output.Pack.Frames {
		actual += MessagePoolRootByteCount(frame.MessageBytes)
	}
	if frame := output.Pack.ContractFrame; frame != nil {
		actual += MessagePoolRootByteCount(frame.MessageBytes)
	}
	paid := output.receiveMemoryOwned && output.ingressSequence == sequence &&
		owner.receiveItem.memoryBudget == sequence.receiveQueue.budget && owner.receiveItem.queueByteCount == actual
	root.admissionLock.Unlock()
	wire = nil
	if !paid {
		output.messagePoolReturn()
		self.t.Fatal("provider fixture wire, roots and decoded owner were not exactly paid before handoff")
	}
	return output
}

// The original, its predecessor promise and its repair escrow are distinct
// reservations. Repair custody lasts until the original roots really return.
func assertReliableIngressRepairCustody(t *testing.T, sequence *ReceiveSequence, item *receiveItem) ByteCount {
	t.Helper()
	budget := sequence.receiveQueue.budget
	noContract := sequence.client.ContractManager().ReceiveNoContract(sequence.source.SourceId)
	want := sequence.receiveBufferSettings.receiveHeldHeadRepairByteCount(noContract)
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	owner := item.decodedOwner
	state := sequence.heldHeadRepair
	if owner == nil || item != &owner.receiveItem || item.memoryBudget != budget || !item.committed ||
		!owner.receivePack.receiveMemoryOwned || !owner.receivePack.heldHeadRepairEligible ||
		owner.receivePack.ingressSequence != sequence || state.owner != nil || state.eligibleCount != 1 ||
		want <= 0 || state.reservedByteCount != want {
		t.Fatalf("selective lease lacks exact decoded-owner repair custody: owner=%t committed=%t metadata=%d want=%d eligible=%d",
			owner != nil, item.committed, state.reservedByteCount, want, state.eligibleCount)
	}
	return want
}

// Direct Pack construction cannot promise duplicate repair against a physical
// owner it never acquired. This preserves the precise rejected old premise.
func TestProviderReliableTcpSelectiveLeaseRequiresDecodedOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			budget := NewTransferMemoryBudget(kib(64))
			t.Cleanup(func() {
				if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
					t.Error("direct fixture left receive ownership after cancellation")
				}
			})
			f := newReliableTcpIngressFixture(t, version, nil)
			f.establishForControl()
			sequence := f.runningReceiveSequence(reliableIngressBudgetSettings(budget))
			pack, id := f.uploadPack(sequence, 1, 101, []byte("direct construction cannot fund repair custody"))
			if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
				pack.messagePoolReturn()
				t.Fatalf("direct fixture did not reach the commitment boundary: accepted=%t err=%v", ok, err)
			}
			synctest.Wait()
			held := sequence.receiveQueue.GetByMessageId(id)
			if held == nil || held.decodedOwner != nil || held.committed || !held.deliveryPrepared || held.deliveryPrepaid <= 0 ||
				held.memoryBudget != budget || budget.UsedByteCount() != held.queueByteCount || budget.Available() <= 0 ||
				sequence.holdPromise.set || sequence.holdPromise.reservedByteCount != 0 ||
				sequence.heldHeadRepair.eligibleCount != 0 || sequence.heldHeadRepair.reservedByteCount != 0 {
				t.Fatalf("ipv%d direct construction did not isolate missing decoded custody", version)
			}
			if _, ok := sequence.receiveHeldHeadRepairLimit(held); ok {
				t.Fatal("ownerless retained item was permitted to promise head repair")
			}
			ack := sequence.ackWindow.Snapshot(false)
			if ack.ackUpdateCount != 0 || len(ack.selectiveAcks) != 0 || sequence.ctx.Err() != nil {
				t.Fatal("ownerless item gained feedback or canceled a healthy sequence")
			}
			t.Logf("ipv%d direct old premise: prepaid=%d retained=%d spare=%d owner=nil committed=false repair=0",
				version, held.deliveryPrepaid, held.queueByteCount, budget.Available())
		})
	}
}

// The policies that acknowledge on admission refuse a direct Pack outright:
// it cannot fund repair escrow. That refusal must leave no queue reservation
// behind. The old order reserved the item first, and once refused nothing
// owned that reservation; the receive root stayed charged after every join.
func TestProviderReliableTcpRefusedDirectHoldReturnsItsReservation(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, policy := range []ReceiveHoldPolicyKind{ReceiveHoldRefuse, ReceiveHoldEvict} {
		for _, version := range []int{4, 6} {
			synctest.Test(t, func(t *testing.T) {
				budget := NewTransferMemoryBudget(kib(64))
				f := newReliableTcpIngressFixture(t, version, nil)
				f.establishForControl()
				sequence := f.runningReceiveSequence(reliableIngressBudgetSettings(budget), func(settings *ReceiveBufferSettings) {
					settings.ReceiveHoldPolicy = policy
				})
				before := f.client.ReceiveStats()
				pack, id := f.uploadPack(sequence, 1, 101, []byte("direct construction cannot fund repair custody"))
				if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
					pack.messagePoolReturn()
					t.Fatalf("direct fixture did not reach the hold: accepted=%t err=%v", ok, err)
				}
				synctest.Wait()
				after := f.client.ReceiveStats()
				if sequence.receiveQueue.GetByMessageId(id) != nil || after.ReceiveQueueDropCount != before.ReceiveQueueDropCount+1 {
					t.Fatalf("policy=%d ipv%d: direct Pack was not refused at the hold", policy, version)
				}
				if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() ||
					sequence.holdPromise.reservedByteCount != 0 ||
					sequence.heldHeadRepair.eligibleCount != 0 || sequence.heldHeadRepair.reservedByteCount != 0 {
					t.Fatalf("policy=%d ipv%d: refused direct Pack kept a receive reservation: used=%d reserved=%d released=%d",
						policy, version, budget.UsedByteCount(), budget.reservedByteCount.Load(), budget.releasedByteCount.Load())
				}
				ack := sequence.ackWindow.Snapshot(false)
				if ack.ackUpdateCount != 0 || len(ack.selectiveAcks) != 0 || sequence.ctx.Err() != nil {
					t.Fatal("refused direct Pack gained feedback or canceled a healthy sequence")
				}
			})
		}
	}
}

// Canceling one exact lease returns its roots and repair escrow while a sibling
// remains committed under the same full root and can still deliver its packet.
func TestProviderReliableTcpSelectiveLeaseCancellationKeepsSibling(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			root := NewTransferMemoryBudget(kib(256))
			left := NewTransferMemoryBudgetWithParent(kib(64), root)
			right := NewTransferMemoryBudgetWithParent(kib(64), root)
			t.Cleanup(func() {
				for _, budget := range []*TransferMemoryBudget{root, left, right} {
					if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
						t.Error("sibling cancellation left a receive root or repair reservation")
					}
				}
			})
			first := newReliableTcpIngressFixture(t, version, nil)
			second := newReliableTcpIngressFixture(t, version, nil)
			first.establishForControl()
			second.establishForControl()
			canceled := first.runningReceiveSequence(reliableIngressBudgetSettings(left))
			sibling := second.runningReceiveSequence(reliableIngressBudgetSettings(right))
			firstId := first.packOwned(canceled, 1, []byte("cancel only this receive owner"))
			siblingPayload := []byte("healthy sibling retains its exact lease")
			secondId := second.packOwned(sibling, 1, siblingPayload)
			synctest.Wait()
			firstHeld := canceled.receiveQueue.GetByMessageId(firstId)
			secondHeld := sibling.receiveQueue.GetByMessageId(secondId)
			if firstHeld == nil || secondHeld == nil {
				t.Fatal("sibling fixture did not retain both originals")
			}
			assertReliableIngressRepairCustody(t, canceled, firstHeld)
			assertReliableIngressRepairCustody(t, sibling, secondHeld)
			siblingBytes := right.UsedByteCount()
			fill := root.Available()
			if !root.TryReserve(fill) || root.Available() != 0 {
				t.Fatal("could not fill shared root around both exact leases")
			}
			defer root.Release(fill)
			canceled.Cancel()
			canceled.WaitForExit()
			synctest.Wait()
			if left.UsedByteCount() != 0 || canceled.heldHeadRepair.reservedByteCount != 0 || canceled.heldHeadRepair.eligibleCount != 0 ||
				right.UsedByteCount() != siblingBytes || root.UsedByteCount() != fill+siblingBytes || sibling.ctx.Err() != nil ||
				sibling.receiveQueue.GetByMessageId(secondId) != secondHeld {
				t.Fatal("canceling one lease changed its healthy sibling or retained canceled custody")
			}
			assertReliableIngressRepairCustody(t, sibling, secondHeld)
			controlId := second.packOwned(sibling, 0, nil)
			synctest.Wait()
			if present, head := reliableIngressCumulativeHead(sibling); !present || head.messageId != controlId {
				t.Fatal("healthy sibling could not admit its promised predecessor")
			}
			(<-second.tcp.sendItems).release()
			synctest.Wait()
			select {
			case item := <-second.tcp.sendItems:
				if !bytes.Equal(item.tcp.payload, siblingPayload) {
					t.Error("healthy sibling admitted different bytes")
				}
				item.release()
			default:
				t.Fatal("healthy sibling did not retain its original through final admission")
			}
			if present, head := reliableIngressCumulativeHead(sibling); !present || head.messageId != secondId ||
				right.UsedByteCount() != 0 || root.UsedByteCount() != fill {
				t.Fatal("healthy sibling failed to finish its exact lease after peer cancellation")
			}
		})
	}
}
