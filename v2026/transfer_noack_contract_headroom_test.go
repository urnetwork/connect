package connect

// FILLFRACTION.md. Why acknowledged traffic stops at the fill
// (ContractFillFraction, 0.9) and leaves headroom, in the owner's words: "no
// ack messages may arrive out of order with new contracts. New contracts are
// sent with ack. So the receiver needs some spare capacity in the previous
// contract to wait to keep acceping no ack while waiting potentially for the
// next contract."
//
// A receiver drops a no-acknowledgement Pack that names a contract it has not
// been introduced to ("drop nack contract mismatch"), and nothing retries it.
// So during a switch, no-acknowledgement Packs need a contract the receiver
// already holds, with room left: the predecessor's headroom.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The root cause, at the receiver, on its real no-acknowledgement path.
//
// Acknowledged traffic has charged the predecessor up to the sender's fill.
// The successor's acknowledged introduction is on its way but has not
// arrived. No-acknowledgement Packs keep coming, out of order with that
// introduction:
//   - a Pack that names the successor is dropped, because the receiver does
//     not hold it yet;
//   - a Pack that names the predecessor is accepted only if the predecessor
//     has room left. At fill 1.0 it has none and the Pack is dropped; at 0.9
//     the headroom admits it.
//
// Once the introduction lands, a late Pack that still names the predecessor is
// accepted at 0.9, because the receiver keeps the superseded predecessor open.
// Without headroom no contract the receiver accepts can carry no-acknowledgement
// traffic during a switch; with headroom the predecessor carries it.
func TestAReceiverDropsANoAckThatOvertakesItsContractUnlessThePredecessorHasHeadroom(t *testing.T) {
	const datagram = ByteCount(1200)
	type outcome struct {
		overtaking, onPredecessor, lateOnPredecessor, onSuccessor bool
	}
	run := func(t *testing.T, fraction float32) outcome {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		receiver, sequence, sourceId := newContractAheadTestReceiveSequence(t, ctx)
		predecessorFrame := contractAheadTestContractFrame(t, receiver, sourceId, receiver.ClientId())
		predecessor := contractAheadTestContractId(t, predecessorFrame)
		successorFrame := contractAheadTestContractFrame(t, receiver, sourceId, receiver.ClientId())
		successor := contractAheadTestContractId(t, successorFrame)

		// the predecessor is introduced and current
		if err := sequence.registerContracts(&receiveItem{contractFrame: predecessorFrame}); err != nil {
			t.Fatalf("introduce the predecessor: %v", err)
		}
		// acknowledged traffic charged it to the sender's fill
		fill := ByteCount(float32(mib(1)) * fraction)
		if !sequence.updateContract(&receiveItem{
			transferItem: transferItem{messageByteCount: fill},
			contractId:   &predecessor,
		}) {
			t.Fatalf("the predecessor refused its acknowledged traffic up to the fill (%d bytes)", fill)
		}
		nack := func(contractId Id) bool {
			accepted, err := sequence.receiveNack(&ReceivePack{
				Pack: &protocol.Pack{
					MessageId:  NewId().Bytes(),
					Nack:       true,
					ContractId: contractId.Bytes(),
				},
				MessageByteCount: datagram,
			})
			if err != nil {
				t.Fatalf("the no-ack path returned an error: %v", err)
			}
			return accepted
		}

		var result outcome
		// it overtook the successor's acknowledged introduction
		result.overtaking = nack(successor)
		result.onPredecessor = nack(predecessor)
		// the introduction lands: the successor becomes current, and the
		// predecessor is superseded but kept open
		if err := sequence.registerContracts(&receiveItem{contractFrame: successorFrame}); err != nil {
			t.Fatalf("introduce the successor: %v", err)
		}
		result.lateOnPredecessor = nack(predecessor)
		result.onSuccessor = nack(successor)
		return result
	}

	withoutHeadroom := run(t, 1.0)
	withHeadroom := run(t, 0.9)

	dropped := func(o outcome) int {
		count := 0
		for _, accepted := range []bool{o.overtaking, o.onPredecessor, o.lateOnPredecessor} {
			if !accepted {
				count += 1
			}
		}
		return count
	}
	t.Logf(
		"fill 1.0: %d of 3 no-ack Packs sent during the switch were dropped by the receiver; fill 0.9: %d of 3",
		dropped(withoutHeadroom), dropped(withHeadroom),
	)
	for _, o := range []outcome{withoutHeadroom, withHeadroom} {
		if o.overtaking {
			t.Error("the receiver accepted a no-ack Pack naming a contract it had not been introduced to")
		}
		if !o.onSuccessor {
			t.Error("the introduced successor refused a no-ack Pack")
		}
	}
	if withoutHeadroom.onPredecessor || withoutHeadroom.lateOnPredecessor {
		t.Error("at fill 1.0 the predecessor, charged to its signed size, still admitted a no-ack Pack")
	}
	if !withHeadroom.onPredecessor {
		t.Error("at fill 0.9 the receiver dropped a no-ack Pack on the predecessor while the successor was being introduced: the headroom did not carry it")
	}
	if !withHeadroom.lateOnPredecessor {
		t.Error("at fill 0.9 the receiver dropped a no-ack Pack on the predecessor that arrived after the successor's introduction: the superseded predecessor was not kept open")
	}
}

// The owner's case end to end, through the public send entry, between two
// real clients.
//
// Every acknowledged Pack that introduces a contract after the opening one
// (an announcement, or a Pack carrying the new contract.s frame) is held
// in the network for fifteen seconds. No-acknowledgement Packs are not held, so
// they arrive out of order with the new contract. Acknowledged traffic fills
// the opening contract to its fill, charged identically on both sides, since
// sending it would queue behind the held announcement on the acknowledged
// lane. One acknowledged Pack then crosses the fill and moves the sequence to
// the successor. A burst of datagrams follows, each with a 300 ms
// budget, while the successor is still being introduced.
//
// At 1.0 the predecessor has no room, and the receiver does not hold the
// successor yet. No contract the receiver accepts can carry the burst, so it
// waits at the boundary, is dropped at its deadline, and never reaches the
// receiver. At the default 0.9 the burst rides the predecessor's headroom
// under a contract the receiver holds: every datagram is delivered, none
// waits and none is promoted. Once the successor is acknowledged, the next
// datagrams advance to it.
func TestNoAckRidesThePredecessorsHeadroomThroughADelayedContractIntroduction(t *testing.T) {
	assertMessagePoolOwnership(t)
	if fraction := DefaultSendBufferSettings().ContractFillFraction; fraction != 0.9 {
		t.Errorf("the default fill fraction is %.2f, want 0.9", fraction)
	}
	const burst = 20
	const after = 3
	type outcome struct {
		delivered          int
		onWire             int
		droppedAtReceiver  int
		namedPredecessor   int
		afterOnSuccessor   int
		afterDelivered     int
		held               uint64
		expired            uint64
		promoted           uint64
		delayedIntroducers int
	}
	run := func(fraction float32) outcome {
		var result outcome
		synctest.Test(t, func(t *testing.T) {
			f := newNoAckAdvanceFixture(t, func(settings *ClientSettings) {
				settings.SendBufferSettings.ContractFillFraction = fraction
				// every contract the same size, so the first switch is the
				// opening contract's
				settings.ContractManagerSettings.InitialContractTransferByteCount = kib(256)
				settings.ContractManagerSettings.StandardContractTransferByteCount = kib(256)
			})
			defer f.close()
			// every introduction after the opening contract's is delayed,
			// starting with the announcement the opening makes when it is set
			f.stateLock.Lock()
			f.introductionDelay = 15 * time.Second
			f.stateLock.Unlock()
			f.finishSend(f.startSend("opening"))
			sender, receiverSequence := f.sequences()
			opening := sender.sendContract
			if opening == nil || !sender.sendContractAcked {
				t.Fatal("the opening contract is not acknowledged")
			}
			if receiverSequence == nil || receiverSequence.openReceiveContracts[opening.contractId] == nil {
				t.Fatal("the receiver does not hold the opening contract")
			}

			// acknowledged traffic up to just below the fill, the same bytes on
			// both sides
			payload := strings.Repeat("x", 4000)
			charge := opening.effectiveTransferByteCount - ByteCount(len(payload))/2 -
				(opening.ackedByteCount + opening.unackedByteCount)
			opening.ackedByteCount += charge
			if opening.noAckBudgetPublished {
				opening.noAckRemainingByteCount.Store(max(0, int64(opening.effectiveTransferByteCount-(opening.ackedByteCount+opening.unackedByteCount))))
			}
			receiverSequence.openReceiveContracts[opening.contractId].ackedByteCount += charge
			// one acknowledged Pack crosses the fill; its acknowledgement waits
			// for the held introduction
			pending := f.startSend(fmt.Sprintf("rel-switch-%s", payload))
			synctest.Wait()
			if sender.sendContract == opening {
				t.Fatal("the acknowledged Pack past the fill did not move the sequence to the successor")
			}
			if sender.sendContractAcked {
				t.Fatal("the successor was acknowledged before the burst, so this row reads nothing")
			}

			// the burst, while the successor is being introduced
			datagram := strings.Repeat("y", 1000)
			for i := range burst {
				_ = f.waitSend(f.startSendWithTimeout(fmt.Sprintf("burst-%03d-%s", i, datagram), 300*time.Millisecond, NoAck()))
				time.Sleep(20 * time.Millisecond)
			}
			// the introduction lands and the switching Pack is acknowledged
			f.finishSend(pending)
			f.settle()
			successor := sender.sendContract
			for i := range after {
				f.finishSend(f.startSendWithTimeout(fmt.Sprintf("after-%03d-%s", i, datagram), 10*time.Second, NoAck()))
			}

			f.stateLock.Lock()
			wireByContent := map[string]noAckAdvanceWireRecord{}
			for _, observed := range f.wire {
				if strings.HasPrefix(observed.content, "burst-") || strings.HasPrefix(observed.content, "after-") {
					wireByContent[observed.content] = observed
				}
			}
			for i := range burst {
				content := fmt.Sprintf("burst-%03d-%s", i, datagram)
				delivered := 0 < f.deliveries[content]
				if delivered {
					result.delivered += 1
				}
				if observed, ok := wireByContent[content]; ok {
					result.onWire += 1
					if !delivered {
						result.droppedAtReceiver += 1
					}
					if observed.hasContractId && observed.contractId == opening.contractId {
						result.namedPredecessor += 1
					}
				}
			}
			for i := range after {
				content := fmt.Sprintf("after-%03d-%s", i, datagram)
				if 0 < f.deliveries[content] {
					result.afterDelivered += 1
				}
				if observed, ok := wireByContent[content]; ok && observed.hasContractId &&
					successor != nil && observed.contractId == successor.contractId {
					result.afterOnSuccessor += 1
				}
			}
			result.delayedIntroducers = f.delayedIntroductions
			f.stateLock.Unlock()
			stats := f.sender.SendRecoveryStats()
			result.held = stats.NoAckContractHoldCount
			result.expired = stats.NoAckContractExpiredCount
			result.promoted = stats.NoAckContractPromotedCount
		})
		return result
	}

	withoutHeadroom := run(1.0)
	atDefault := run(0.9)
	t.Logf(
		"fill 1.0: %d of %d burst datagrams reached the receiver (%d held at the boundary, %d dropped at their deadline); "+
			"fill 0.9: %d of %d reached it (%d held, %d dropped), %d named the predecessor",
		withoutHeadroom.delivered, burst, withoutHeadroom.held, withoutHeadroom.expired,
		atDefault.delivered, burst, atDefault.held, atDefault.expired, atDefault.namedPredecessor,
	)

	if withoutHeadroom.delayedIntroducers == 0 || atDefault.delayedIntroducers == 0 {
		t.Fatal("no contract introduction was delayed, so the burst never met an introduction in flight")
	}
	if burst <= withoutHeadroom.delivered {
		t.Errorf("at fill 1.0 all %d burst datagrams were delivered: the switch had room this row did not expect", burst)
	}
	if withoutHeadroom.expired == 0 {
		t.Error("at fill 1.0 no burst datagram was dropped at its deadline while the successor was being introduced")
	}
	if atDefault.delivered != burst {
		t.Errorf("at fill 0.9, %d of %d burst datagrams reached the receiver: the predecessor's headroom did not carry them through the delayed introduction", atDefault.delivered, burst)
	}
	if atDefault.held != 0 || atDefault.expired != 0 {
		t.Errorf("at fill 0.9, %d burst datagrams waited at the boundary and %d were dropped at their deadline", atDefault.held, atDefault.expired)
	}
	if atDefault.namedPredecessor != burst {
		t.Errorf("at fill 0.9, %d of %d burst datagrams named the predecessor, the contract the receiver held", atDefault.namedPredecessor, burst)
	}
	for _, o := range []outcome{withoutHeadroom, atDefault} {
		if o.droppedAtReceiver != 0 {
			t.Errorf("%d datagrams reached the receiver naming a contract it did not hold", o.droppedAtReceiver)
		}
		if o.promoted != 0 {
			t.Errorf("%d datagrams were promoted onto the acknowledged lane", o.promoted)
		}
	}
	if atDefault.afterDelivered != after || atDefault.afterOnSuccessor != after {
		t.Errorf("after the introduction, %d of %d datagrams were delivered and %d named the acknowledged successor", atDefault.afterDelivered, after, atDefault.afterOnSuccessor)
	}
}

// The hold, pinned to its one remaining cause: the headroom is used up before
// the successor is acknowledged. Until then the predecessor's headroom carries
// every no-acknowledgement Pack without a hold. Once the successor is
// acknowledged, no-acknowledgement traffic advances to it and the predecessor
// is retired.
func TestANoAckPackHoldsOnlyWhenTheHeadroomIsExhaustedBeforeTheSuccessorIsAcknowledged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, sequence, destinationId, predecessor := newSendNoContractHarness(t, ctx)
	sequence.sendContractEverAcknowledged = true
	sequence.sendContractMetadataGeneration = sequence.contractMetadata().generation
	// at 0.9: acknowledged traffic charged the predecessor to its fill
	predecessor.effectiveTransferByteCount = predecessor.transferByteCount * 9 / 10
	predecessor.ackedByteCount = predecessor.effectiveTransferByteCount
	// and moved to the successor, which is being introduced
	successor := newContractAheadTestContract(t, client, destinationId)
	successor.effectiveTransferByteCount = successor.transferByteCount * 9 / 10
	sequence.setContract(successor, sequence.contractMetadata().generation)
	if sequence.noAckHeadroomContract != predecessor {
		t.Fatal("the switch retired the acknowledged predecessor, so its headroom is gone")
	}
	if sequence.sendContractAcked {
		t.Fatal("the successor is already acknowledged, so this row reads nothing")
	}

	const datagram = ByteCount(1000)
	headroom := predecessor.transferByteCount - predecessor.effectiveTransferByteCount
	carried := ByteCount(0)
	for ; carried+datagram <= headroom; carried += datagram {
		updated, held, err := sequence.updateContractNoAckAdvanceOutcome(datagram)
		if err != nil || !updated || held {
			t.Fatalf("with %d of %d headroom bytes left a no-ack Pack was not carried: updated=%t held=%t err=%v", headroom-carried, headroom, updated, held, err)
		}
		if sequence.noAckHeadroomCharge != predecessor {
			t.Fatal("the Pack was not charged to the predecessor's headroom")
		}
		// written: settled the way ackItem settles a no-ack item
		sequence.noAckHeadroomCharge = nil
		predecessor.ack(datagram)
	}
	updated, held, err := sequence.updateContractNoAckAdvanceOutcome(datagram)
	if err != nil || updated || !held {
		t.Fatalf("with the headroom used up the Pack did not wait for the successor: updated=%t held=%t err=%v", updated, held, err)
	}
	if used := predecessor.ackedByteCount + predecessor.unackedByteCount; used > predecessor.transferByteCount {
		t.Fatalf("the predecessor carries %d bytes, past its signed %d", used, predecessor.transferByteCount)
	}

	// the successor is acknowledged
	sequence.setContractAcked(successor, true)
	if sequence.noAckHeadroomContract != nil {
		t.Fatal("the predecessor kept its headroom after the successor was acknowledged")
	}
	updated, held, err = sequence.updateContractNoAckAdvanceOutcome(datagram)
	if err != nil || !updated || held || sequence.noAckHeadroomCharge != nil {
		t.Fatalf("after the acknowledgement the Pack did not ride the successor: updated=%t held=%t err=%v", updated, held, err)
	}
	if successor.ackedByteCount+successor.unackedByteCount < datagram {
		t.Fatal("the Pack was not debited to the successor")
	}
}
