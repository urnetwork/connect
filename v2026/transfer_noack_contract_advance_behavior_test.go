package connect

// FILLFRACTION.md, the behaviour the root-cause rows in
// transfer_noack_contract_advance_test.go pin, read through the counters the
// fleet will see: a fast-path flow brings its successor's announcement due, a
// lost announcement holds the boundary's Pack until it is retried or the
// caller's budget runs out, mixed traffic keeps its order and its accounting,
// the opening keeps its promotion, and the fill fraction's default is chosen
// from what the same traffic costs at each value.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// A sequence written only on the fast path is charged on the owner's passes,
// and the owner passes only when woken. Above the announcement threshold the
// owner arms the callers' wake at it, so the reservation that crosses it
// brings the owner back to announce. Before the change nothing armed it, and
// such a flow reached every boundary unannounced.
func TestAFastPathReservationAtTheAnnounceThresholdWakesTheOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	harness := newNoAckFastPathHarness(t, ctx, 4)
	sequence := harness.sequence
	sequence.contractAheadSupported.Store(true)
	threshold := sequence.announceAheadByteCount()
	// a contract with twice the threshold left, so the announcement is not due
	contract := harness.contract
	contract.transferByteCount = 4 * threshold
	contract.effectiveTransferByteCount = 4 * threshold
	contract.ackedByteCount = 2 * threshold
	// the callers' shared room, in the same terms
	contract.noAckRemainingByteCount.Store(int64(2 * threshold))

	sequence.maybeAnnounceContractAhead()
	if level := contract.successorCheckRemainingByteCount.Load(); level != int64(threshold) {
		t.Fatalf("the callers' wake is armed at %d, want the announcement threshold %d", level, threshold)
	}
	// drain any wake already pending, so the one below is the reservation's
	select {
	case <-sequence.noAckFastPathSettled:
	default:
	}
	snapshot := sequence.noAckFastPath.Load()
	if snapshot == nil {
		t.Fatal("no fast path is published")
	}
	if !snapshot.reserve(threshold + 1) {
		t.Fatal("the reservation that crosses the threshold was refused")
	}
	select {
	case <-sequence.noAckFastPathSettled:
	default:
		t.Fatal("a reservation crossed the announcement threshold without waking the owner")
	}
	snapshot.release(threshold + 1)
}

// A lost announcement. The boundary's Pack waits, uncharged, while the
// announcement is retried; with budget to spare it is written on the
// successor once the retry is acknowledged, and nothing is promoted.
func TestALostAnnouncementHoldsTheBoundaryPackUntilItsRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNoAckAdvanceFixture(t)
		defer f.close()
		f.dropAnnouncements = 1
		f.finishSend(f.startSend("opening"))

		const count = 40
		payload := strings.Repeat("x", 4000)
		for i := range count {
			f.finishSend(f.startSendWithTimeout(fmt.Sprintf("noack-%03d-%s", i, payload), 10*time.Second, NoAck()))
		}

		f.stateLock.Lock()
		dropped := f.droppedAnnouncements
		for i := range count {
			if n := f.deliveries[fmt.Sprintf("noack-%03d-%s", i, payload)]; n != 1 {
				t.Errorf("no-ack message %d was delivered %d times, want once", i, n)
			}
		}
		f.stateLock.Unlock()
		if dropped != 1 {
			t.Fatalf("the fixture dropped %d announcements, want one", dropped)
		}
		stats := f.sender.SendRecoveryStats()
		if stats.NoAckContractHoldCount == 0 {
			t.Error("no pack waited at the boundary whose announcement was lost")
		}
		if stats.NoAckContractExpiredCount != 0 {
			t.Errorf("%d held packs expired with budget to spare", stats.NoAckContractExpiredCount)
		}
		if stats.NoAckContractPromotedCount != 0 {
			t.Errorf("%d no-ack packs were promoted while the announcement was retried", stats.NoAckContractPromotedCount)
		}
		if stats.NoAckContractAdvanceCount == 0 {
			t.Error("no no-ack pack advanced the sequence")
		}
	})
}

// The same loss with a caller's budget shorter than the retry: the held Pack
// is dropped at its deadline, unwritten and uncharged, counted as expired at a
// boundary, and the traffic after it advances once the retry lands.
func TestAHeldBoundaryPackExpiresUnchargedAtItsDeadline(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNoAckAdvanceFixture(t)
		defer f.close()
		f.dropAnnouncements = 1
		f.finishSend(f.startSend("opening"))

		payload := strings.Repeat("x", 4000)
		expired := ""
		sent := []string{}
		for i := 0; expired == "" && i < 64; i++ {
			content := fmt.Sprintf("noack-%03d-%s", i, payload)
			err := f.waitSend(f.startSendWithTimeout(content, 500*time.Millisecond, NoAck()))
			if err != nil {
				expired = content
				continue
			}
			sent = append(sent, content)
		}
		if expired == "" {
			t.Fatal("no boundary pack expired, so the fixture never held one")
		}
		// past the announcement's retry, which is what lets the rest advance
		time.Sleep(3 * time.Second)
		synctest.Wait()
		for i := range 8 {
			content := fmt.Sprintf("after-%03d-%s", i, payload)
			f.finishSend(f.startSendWithTimeout(content, 10*time.Second, NoAck()))
			sent = append(sent, content)
		}

		f.stateLock.Lock()
		if n := f.deliveries[expired]; n != 0 {
			t.Errorf("the expired pack was delivered %d times", n)
		}
		for _, content := range sent {
			if n := f.deliveries[content]; n != 1 {
				t.Errorf("a no-ack message was delivered %d times, want once", n)
			}
		}
		for _, observed := range f.wire {
			if observed.content == expired {
				t.Error("the expired pack reached the wire")
			}
		}
		f.stateLock.Unlock()
		// the receiver's acknowledgement timers, for the control packs last
		f.settle()
		stats := f.sender.SendRecoveryStats()
		if stats.NoAckContractExpiredCount != 1 {
			t.Errorf("%d held packs were counted as expired, want one", stats.NoAckContractExpiredCount)
		}
		if stats.NoAckContractPromotedCount != 0 {
			t.Errorf("%d no-ack packs were promoted", stats.NoAckContractPromotedCount)
		}
		sender, _ := f.sequences()
		for _, contract := range sender.openSendContracts {
			if contract.unackedByteCount != 0 {
				t.Errorf("contract %s holds %d unacknowledged bytes; the expired pack leaked a debit", contract.contractId, contract.unackedByteCount)
			}
		}
	})
}

// Acknowledged and no-acknowledgement traffic interleaved across several
// boundaries: the acknowledged messages arrive in their order, each exactly
// once, every no-ack message arrives once and none is promoted, and each
// contract's two parties close it with the same count, which is what a switch
// made out of order at the receiver would break.
func TestMixedTrafficKeepsOrderAndAccountingAcrossContractBoundaries(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNoAckAdvanceFixture(t)
		defer f.close()
		payload := strings.Repeat("x", 4000)
		reliable := []string{}
		datagrams := []string{}
		for i := range 180 {
			if i%3 == 0 {
				content := fmt.Sprintf("rel-%03d-%s", i, payload)
				reliable = append(reliable, content)
				f.finishSend(f.startSend(content))
			} else {
				content := fmt.Sprintf("noack-%03d-%s", i, payload)
				datagrams = append(datagrams, content)
				f.finishSend(f.startSend(content, NoAck()))
			}
		}

		f.stateLock.Lock()
		order := append([]string{}, f.deliveryOrder...)
		for _, content := range append(append([]string{}, reliable...), datagrams...) {
			if n := f.deliveries[content]; n != 1 {
				t.Errorf("a message was delivered %d times, want once", n)
			}
		}
		promoted := 0
		contractIds := map[Id]bool{}
		for _, observed := range f.wire {
			if strings.HasPrefix(observed.content, "noack-") {
				if !observed.nack {
					promoted += 1
				} else if observed.hasContractId {
					contractIds[observed.contractId] = true
				}
			}
		}
		f.stateLock.Unlock()
		next := 0
		for _, content := range order {
			if !strings.HasPrefix(content, "rel-") {
				continue
			}
			if next < len(reliable) && content == reliable[next] {
				next += 1
				continue
			}
			t.Fatalf("reliable messages arrived out of order at %d", next)
		}
		if next != len(reliable) {
			t.Fatalf("%d of %d reliable messages arrived", next, len(reliable))
		}
		if promoted != 0 {
			t.Errorf("%d no-ack messages crossed the wire on the acknowledged lane", promoted)
		}
		if len(contractIds) < 4 {
			t.Errorf("no-ack traffic used %d contracts, want at least four", len(contractIds))
		}

		f.close()
		senderReports := f.authority.terminalReportsByContract()
		receiverReports := f.reverseAuthority.terminalReportsByContract()
		compared := 0
		for contractId, sent := range senderReports {
			received, ok := receiverReports[contractId]
			if !ok {
				continue
			}
			compared += 1
			if sent != received {
				t.Errorf("contract %s closed with %d bytes at the sender and %d at the receiver", contractId, sent, received)
			}
		}
		if compared < 3 {
			t.Errorf("only %d contracts were closed by both parties", compared)
		}
	})
}

// The opening, pinned as the one place a no-acknowledgement Pack is still
// promoted: no contract of the sequence is acknowledged yet, so there is
// nothing for it to ride, and riding the opening head in order costs no round
// trip. Once the opening is acknowledged nothing promotes.
func TestTheOpeningIsTheOnlyPromotion(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNoAckAdvanceFixture(t)
		defer f.close()
		payload := strings.Repeat("x", 4000)
		for i := range 60 {
			f.finishSend(f.startSend(fmt.Sprintf("noack-%03d-%s", i, payload), NoAck()))
		}
		f.stateLock.Lock()
		promotedContents := []string{}
		for _, observed := range f.wire {
			if strings.HasPrefix(observed.content, "noack-") && !observed.nack {
				promotedContents = append(promotedContents, observed.content)
			}
		}
		delivered := 0
		for i := range 60 {
			delivered += f.deliveries[fmt.Sprintf("noack-%03d-%s", i, payload)]
		}
		f.stateLock.Unlock()
		if delivered != 60 {
			t.Errorf("%d of 60 no-ack messages were delivered", delivered)
		}
		if len(promotedContents) != 1 || !strings.HasPrefix(promotedContents[0], "noack-000-") {
			t.Errorf("%d no-ack messages were promoted, want only the one that opened the sequence", len(promotedContents))
		}
		if count := f.sender.SendRecoveryStats().NoAckContractPromotedCount; count != 1 {
			t.Errorf("%d promotions were counted, want the opening's one", count)
		}
	})
}

// The fill fraction's semantics on the same no-acknowledgement traffic, at
// the default 0.9 and with no headroom at 1.0. At 0.9 the headroom is used:
// every contract the traffic moves past is closed between its fill and its
// signed size, never above. At 1.0 nothing can use it. Either way every
// message is delivered once, none is promoted and none expires. The fixture
// writes in no time, so its boundaries meet the announcement unacknowledged,
// which is exactly when the headroom is read.
// TestNoAckRidesThePredecessorsHeadroomThroughADelayedContractIntroduction
// shows the default's reason at a real switch.
func TestTheDefaultFillLetsNoAckTrafficCarryContractsPastTheFillToTheirSignedSize(t *testing.T) {
	assertMessagePoolOwnership(t)
	if fraction := DefaultSendBufferSettings().ContractFillFraction; fraction != 0.9 {
		t.Fatalf("the default fill fraction is %.2f, want 0.9", fraction)
	}
	type outcome struct {
		contracts int
		holds     uint64
		headroom  uint64
		fills     []float64
	}
	run := func(fraction float32) outcome {
		var result outcome
		synctest.Test(t, func(t *testing.T) {
			f := newNoAckAdvanceFixture(t, func(settings *ClientSettings) {
				settings.SendBufferSettings.ContractFillFraction = fraction
			})
			defer f.close()
			f.finishSend(f.startSend("opening"))
			payload := strings.Repeat("x", 4000)
			const count = 240
			for i := range count {
				f.finishSend(f.startSend(fmt.Sprintf("noack-%03d-%s", i, payload), NoAck()))
			}
			f.stateLock.Lock()
			for i := range count {
				if n := f.deliveries[fmt.Sprintf("noack-%03d-%s", i, payload)]; n != 1 {
					t.Errorf("at fraction %.2f a no-ack message was delivered %d times", fraction, n)
				}
			}
			contractIds := map[Id]bool{}
			for _, observed := range f.wire {
				if strings.HasPrefix(observed.content, "noack-") && observed.nack && observed.hasContractId {
					contractIds[observed.contractId] = true
				}
			}
			signed := map[Id]ByteCount{}
			for contractId, transferByteCount := range f.signedContracts {
				signed[contractId] = transferByteCount
			}
			f.stateLock.Unlock()
			// A hold here is the fixture's: its traffic is written in no time
			// while the receiver's acknowledgement waits for its compression
			// timer, so a boundary can arrive before the announcement's
			// acknowledgement. The Pack then waits for it and rides the
			// successor; none is promoted and none expires.
			stats := f.sender.SendRecoveryStats()
			if stats.NoAckContractPromotedCount != 0 || stats.NoAckContractExpiredCount != 0 {
				t.Errorf("at fraction %.2f: %d promoted, %d expired", fraction, stats.NoAckContractPromotedCount, stats.NoAckContractExpiredCount)
			}
			result.holds = stats.NoAckContractHoldCount
			result.headroom = stats.NoAckContractHeadroomCount
			sender, _ := f.sequences()
			current := sender.sendContract
			f.close()
			for contractId, used := range f.authority.terminalReportsByContract() {
				transferByteCount, ok := signed[contractId]
				if !ok || !contractIds[contractId] || (current != nil && contractId == current.contractId) {
					continue
				}
				result.fills = append(result.fills, float64(used)/float64(transferByteCount))
			}
			result.contracts = len(contractIds)
		})
		return result
	}
	atDefault := run(0.9)
	withoutHeadroom := run(1)
	if len(atDefault.fills) == 0 {
		t.Fatal("no contract was filled and closed at the default")
	}
	if atDefault.headroom == 0 {
		t.Error("at the default no no-ack Pack rode the headroom, though its boundaries met unacknowledged announcements")
	}
	// A contract switched at its fill, its successor already acknowledged,
	// leaves less than one message unused below the fill: at most 4 KiB of the
	// smallest, 64 KiB, contract.
	for _, fill := range atDefault.fills {
		if fill < 0.9-float64(kib(5))/float64(kib(64)) || 1 < fill {
			t.Errorf("a contract the traffic moved past was closed %.1f%% full at the default, want between its fill, less a message, and its signed size", 100*fill)
		}
	}
	if withoutHeadroom.headroom != 0 {
		t.Errorf("at 1.0, %d no-ack Packs were charged to a headroom that does not exist", withoutHeadroom.headroom)
	}
	t.Logf("the same traffic: %d contracts, %d held, %d in the headroom at 0.9; %d contracts, %d held at 1.0", atDefault.contracts, atDefault.holds, atDefault.headroom, withoutHeadroom.contracts, withoutHeadroom.holds)
}
