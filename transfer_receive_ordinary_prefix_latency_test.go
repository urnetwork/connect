// A signed Client decoder sees a real published current DATA owner while its
// worker is held. Ordinary RX custody must prove a consecutive prefix without
// granting proof, ACK, loan or ancestor capacity.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The successor pays its complete owner before the held worker advances.
func TestReceiveConsumerOrdinaryRxPrefixAdmitsContiguousPaidHead(t *testing.T) {
	testReceiveConsumerOrdinaryPrefix(t, "contiguous")
}

// The same non-Head original is admissible once its predecessor leaves.
func TestReceiveConsumerOrdinaryRxPrefixKeepsCurrentOriginalWithoutHead(t *testing.T) {
	testReceiveConsumerOrdinaryPrefix(t, "current")
}

// A paid current position cannot replace an absent middle position.
func TestReceiveConsumerOrdinaryRxPrefixCannotJumpGap(t *testing.T) {
	testReceiveConsumerOrdinaryPrefix(t, "gap")
}

// Physical ceilings refuse before publication or receipt creation.
func TestReceiveConsumerOrdinaryRxPrefixKeepsPhysicalCaps(t *testing.T) {
	for _, mode := range []string{"head-cap", "root-cap"} {
		testReceiveConsumerOrdinaryPrefix(t, mode)
	}
}

// Cancellation retains cleanup responsibility, never new admission authority.
func TestReceiveConsumerOrdinaryRxPrefixKeepsCanceledGeneration(t *testing.T) {
	testReceiveConsumerOrdinaryPrefix(t, "canceled")
}

// Routes and the existing owner hook force the ordering. Every wire position,
// contract, physical allocation and receipt comes from actual Client work.
func testReceiveConsumerOrdinaryPrefix(t *testing.T, mode string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		var cause string
		t.Cleanup(func() {
			if cause != "" {
				t.Error(cause)
			}
		})
		const smallCost, largeCost = ByteCount(1536), ByteCount(5120)
		const rxLimit = smallCost + largeCost
		rootLimit, headLimit := 2*rxLimit, rxLimit
		if mode == "head-cap" {
			headLimit = smallCost
		}
		if mode == "root-cap" {
			rootLimit = rxLimit + smallCost
		}
		root := NewTransferMemoryBudget(rootLimit)
		clientBudget := NewTransferMemoryBudgetWithParent(2*rxLimit, root)
		rx := NewTransferMemoryBudgetWithParent(rxLimit, clientBudget)
		head := NewTransferMemoryBudgetWithParent(headLimit, clientBudget)
		pack := NewTransferMemoryBudgetWithParent(rxLimit, clientBudget)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, clientBudget, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("ordinary-prefix control retained a quota after joined clients")
				}
			}
			t.Log("[ordinary-rx-prefix-cleanup-joined]")
		})
		entered, resume := make(chan struct{}), make(chan struct{})
		var resumeOnce sync.Once
		release := func() { resumeOnce.Do(func() { close(resume) }) }
		var armed atomic.Bool
		var firstId, successorId Id
		var firstOwner *decodedPackOwner
		var firstSequence *ReceiveSequence
		var successorPaidHead, successorCurrent bool
		ownerStage := func(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if stage != "pack_released" || owner == nil {
				return
			}
			if owner.receiveItem.messageId == successorId {
				root.admissionLock.Lock()
				successorPaidHead = owner.receiveItem.memoryBudget == head && owner.receivePack.receiveMemoryOwned &&
					owner.receivePack.ingressSequence == sequence && owner.receivePack.ingressLoanByteCount == 0
				successorCurrent = owner.receiveItem.sequenceNumber == sequence.holdPromise.ingressFloor
				root.admissionLock.Unlock()
			}
			if owner.receiveItem.messageId == firstId && armed.CompareAndSwap(true, false) {
				firstOwner, firstSequence = owner, sequence
				close(entered)
				<-resume
			}
		}
		fixture, logger := newConsumerFinalHandoffFixture(t, rx, head, pack, ownerStage, nil)
		t.Cleanup(release)
		lastApplication := map[uint32]uint64{}
		sentPayload := map[uint32]uint32{}
		write := func(lane uint32, payloadBytes int) *windowRoundFrame {
			packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 19+sentPayload[lane], bytes.Repeat([]byte{0x53}, payloadBytes))
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
			accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
				if err == nil {
					fixture.ackedCount++
				}
			}, time.Second, TransferKey{LogicalLane: lane})
			if !accepted {
				MessagePoolReturn(frame.MessageBytes)
			}
			if !accepted || err != nil {
				t.Fatalf("source refused lane=%d accepted=%t error=%v", lane, accepted, err)
			}
			sentPayload[lane] += uint32(payloadBytes)
			for range cap(fixture.senderOut) {
				wire := fixture.take(fixture.senderOut)
				if wire.pack == nil {
					t.Fatal("ordinary-prefix source did not encode a Pack")
				}
				if wire.pack.LogicalLane != lane {
					continue
				}
				for _, payload := range wire.pack.Frames {
					if payload.MessageType == protocol.MessageType_IpIpPacketFromProvider {
						if last, present := lastApplication[lane]; present && wire.pack.SequenceNumber <= last {
							break
						}
						lastApplication[lane] = wire.pack.SequenceNumber
						return wire
					}
				}
				if len(wire.pack.Frames) == 0 {
					fixture.forward(wire, fixture.receiverIn)
					fixture.acknowledge()
				}
			}
			t.Fatal("ordinary-prefix source exceeded its finite capture")
			return nil
		}
		foreignFirst := write(1, 32)
		first := write(0, 32)
		second := write(0, 1280)
		originals := []*windowRoundFrame{second}
		successor := second
		if mode == "gap" {
			successor = write(0, 1280)
			originals = append(originals, successor)
		}
		foreignTail := write(1, 1280)
		var backups []*windowRoundFrame
		for _, original := range originals {
			MessagePoolShareReadOnly(original.bytes)
			backup := &windowRoundFrame{bytes: original.bytes, pack: original.pack}
			fixture.heldFrames = append(fixture.heldFrames, backup)
			backups = append(backups, backup)
		}
		firstId, successorId = RequireIdFromBytes(first.pack.MessageId), RequireIdFromBytes(successor.pack.MessageId)
		if first.pack.Head || successor.pack.Head || first.pack.Nack || successor.pack.Nack ||
			!bytes.Equal(first.pack.SequenceId, successor.pack.SequenceId) || second.pack.SequenceNumber != first.pack.SequenceNumber+1 {
			t.Fatal("premise requires distinct consecutive original non-Head DATA")
		}
		fixture.forward(foreignTail, fixture.receiverIn)
		if rx.UsedByteCount() != largeCost || head.UsedByteCount() != 0 || fixture.deliveredCount != 0 || fixture.ackedCount != 0 {
			t.Fatal("real foreign tentative tail did not establish exact RX pressure without receipt")
		}
		started := time.Now()
		armed.Store(true)
		fixture.forward(first, fixture.receiverIn)
		select {
		case <-entered:
		default:
			t.Fatal("actual ordinary-RX predecessor did not reach its worker barrier")
		}
		root.admissionLock.Lock()
		prefixOwned := firstOwner != nil && firstSequence != nil && firstOwner.inUse &&
			firstOwner.receiveItem.memoryBudget == rx && firstOwner.receiveItem.QueueByteCount() == smallCost &&
			firstOwner.receivePack.ingressSequence == firstSequence && firstOwner.receivePack.receiveMemoryOwned &&
			firstOwner.receivePack.receivePublishedReliable && firstOwner.receivePack.receiveConsumerData &&
			firstOwner.receivePack.ingressLoanByteCount == 0 && firstSequence.holdPromise.ingressFloor == first.pack.SequenceNumber
		root.admissionLock.Unlock()
		if !prefixOwned || rx.UsedByteCount() != rxLimit || root.UsedByteCount() != rxLimit || head.UsedByteCount() != 0 ||
			fixture.deliveredCount != 0 || fixture.ackedCount != 0 || len(fixture.receiverOut) != 0 {
			t.Fatal("held predecessor lacks published, unloaned, exactly charged ordinary-RX custody")
		}
		if mode == "current" {
			release()
			synctest.Wait()
			fixture.acknowledge()
			if firstSequence.holdPromise.ingressFloor != second.pack.SequenceNumber || rx.UsedByteCount() != largeCost {
				t.Fatal("real predecessor completion did not publish the exact successor floor")
			}
			fixture.forward(second, fixture.receiverIn)
			fixture.acknowledge()
			if !successorPaidHead || !successorCurrent || fixture.deliveredCount != 2 || fixture.ackedCount != 2 || time.Now() != started {
				t.Fatal("same non-Head original failed independently paid exact-current admission")
			}
		} else {
			if mode == "canceled" {
				firstSequence.Cancel()
			}
			dropsBefore := fixture.receiver.receivePackHandoffDropCount.Load()
			fixture.forward(successor, fixture.receiverIn)
			drops := fixture.receiver.receivePackHandoffDropCount.Load() - dropsBefore
			root.admissionLock.Lock()
			var paidSuccessor *ReceivePack
			for owner := firstSequence.holdPromise.ingressHead; owner != nil; owner = owner.ingressNext {
				if owner.decodedOwner != nil && owner.decodedOwner.receiveItem.messageId == successorId {
					paidSuccessor = owner
				}
			}
			paid := paidSuccessor != nil && paidSuccessor.ingressSequence == firstSequence && paidSuccessor.receiveMemoryOwned &&
				paidSuccessor.receivePublishedReliable && paidSuccessor.receiveConsumerData && paidSuccessor.ingressLoanByteCount == 0 &&
				paidSuccessor.decodedOwner.receiveItem.memoryBudget == head && paidSuccessor.decodedOwner.receiveItem.QueueByteCount() == largeCost
			prefixUnchanged := firstOwner.inUse && firstOwner.receiveItem.memoryBudget == rx &&
				firstOwner.receiveItem.messageId == firstId && firstOwner.receiveItem.QueueByteCount() == smallCost &&
				firstSequence.holdPromise.ingressFloor == first.pack.SequenceNumber
			root.admissionLock.Unlock()
			if !prefixUnchanged || time.Now() != started || fixture.deliveredCount != 0 || fixture.ackedCount != 0 ||
				len(fixture.receiverOut) != 0 || rx.UsedByteCount() != rxLimit || root.UsedByteCount() != rxLimit+head.UsedByteCount() {
				t.Fatal("successor offer changed time, prefix custody, ordinary RX or receipt authority")
			}
			if mode == "contiguous" {
				if !paid || head.UsedByteCount() != largeCost || drops != 0 {
					if paidSuccessor != nil || head.UsedByteCount() != 0 || drops != 1 {
						t.Fatal("unexpected ordinary-prefix failure shape")
					}
					cause = "ORDINARY_RX_PAID_CONTIGUOUS_PREFIX_REJECTED_BEFORE_WORKER_ADVANCE"
				}
			} else if paidSuccessor != nil || head.UsedByteCount() != 0 || drops != 1 {
				t.Fatal("gap, physical cap or canceled generation authorized a successor")
			}
			t.Logf("[ordinary-rx-prefix] mode=%s first=%d successor=%d prefix_RX=%d paid_Head=%t refused=%d RX=%d Head=%d root=%d elapsed=%s", mode,
				first.pack.SequenceNumber, successor.pack.SequenceNumber, smallCost, paid, drops, rx.UsedByteCount(), head.UsedByteCount(), root.UsedByteCount(), time.Since(started))
			release()
			synctest.Wait()
			fixture.acknowledge()
			if mode == "canceled" {
				if firstSequence.ctx.Err() == nil || fixture.deliveredCount > 1 {
					t.Fatal("canceled generation delivered its refused successor")
				}
				return
			}
		}
		// Genuine original shares finish cap-negative and old-code cases after
		// the independently held foreign gap drains. No ceiling is changed.
		fixture.forward(foreignFirst, fixture.receiverIn)
		fixture.acknowledge()
		for _, seed := range backups {
			MessagePoolShareReadOnly(seed.bytes)
			reoffer := &windowRoundFrame{bytes: seed.bytes, pack: seed.pack}
			fixture.heldFrames = append(fixture.heldFrames, reoffer)
			fixture.forward(reoffer, fixture.receiverIn)
			fixture.acknowledge()
		}
		want := 3 + len(originals)
		if fixture.deliveredCount != want || fixture.ackedCount != want || rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 ||
			pack.UsedByteCount() != 0 || root.UsedByteCount() != 0 || firstSequence.receiveQueue.Len() != 0 || len(logger.messages()) != 0 {
			t.Fatalf("cleanup lacks exact-once delivery/ACK or refund: mode=%s delivered=%d acked=%d want=%d RX=%d Head=%d Pack=%d root=%d errors=%d", mode,
				fixture.deliveredCount, fixture.ackedCount, want, rx.UsedByteCount(), head.UsedByteCount(), pack.UsedByteCount(), root.UsedByteCount(), len(logger.messages()))
		}
	})
}

// Mutations use actual allocated RX owners under their admission lock. The
// independent live-decoder positive determines whether valid custody works.
func TestReceiveConsumerOrdinaryRxPrefixKeepsExactLivePublishedIdentity(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	fixture := newReceiveRecoveryPrefixBoundsFixture(t, 0)
	owner := fixture.add(t, 0, fixture.rx)
	fixture.root.admissionLock.Lock()
	owner.receivePack.receivePublishedReliable = false
	unpublished := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	owner.receivePack.receivePublishedReliable = true
	owner.receivePack.receiveConsumerData = false
	protocol := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	owner.receivePack.receiveConsumerData = true
	owner.receivePack.ingressLoanByteCount = 1
	loaned := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	owner.receivePack.ingressLoanByteCount = 0
	owner.receivePack.receiveMemoryOwned = false
	unowned := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	owner.receivePack.receiveMemoryOwned = true
	owner.receivePack.ingressSequence = &ReceiveSequence{}
	foreignGeneration := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	owner.receivePack.ingressSequence = fixture.sequence
	fixture.root.admissionLock.Unlock()
	fixture.release(owner)
	returned := fixture.allows(1)
	fixture.add(t, 0, fixture.other)
	foreignBudget := fixture.allows(1)
	if unpublished || protocol || loaned || unowned || foreignGeneration || returned || foreignBudget {
		t.Fatal("unpublished, protocol, loaned, unowned, foreign, returned or differently charged predecessor authorized a prefix")
	}
}
