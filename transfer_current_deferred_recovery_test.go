//go:build urnetwork_synthetic_fixture

// A real first-write cumulative prefix must not leave its newly current
// missing original waiting on deferrals accumulated behind that prefix.
package connect

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type currentDeferredWireFact struct {
	writes, sendCount int
	last              time.Time
	head, resend      bool
}

func TestPublicInitialAckPrefixPromotesAlreadyDeferredCurrentData(t *testing.T) {
	GetMessagePoolAggregateStats()
	var missedProbe bool
	var oldDue, recoveredAfter, probe time.Duration
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const leafBytes = ByteCount(128 * 1024)
		root := NewTransferMemoryBudget(4 * leafBytes)
		tx := NewTransferMemoryBudgetWithParent(leafBytes, root)
		rx := NewTransferMemoryBudgetWithParent(leafBytes, root)
		head := NewTransferMemoryBudgetWithParent(leafBytes, root)
		pack := NewTransferMemoryBudgetWithParent(leafBytes, root)
		budgets := []*TransferMemoryBudget{pack, head, rx, tx, root}
		t.Cleanup(func() {
			for _, budget := range budgets {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("public deferred recovery retained DATA quota after joins")
				}
			}
		})
		var wireLock sync.Mutex
		wireFacts := map[Id]currentDeferredWireFact{}
		layout := consumerPaidPackByteLayout{label: "current-deferred-original", byteLimit: 174762,
			leafBytes: leafBytes, rootBytes: 4 * leafBytes, countLimit: 85}
		fixture, log, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, nil,
			func(receiver bool, settings *ClientSettings) {
				settings.ReceiveBufferSettings.AdvertiseReceiveWindow = true
				if receiver {
					return
				}
				send := settings.SendBufferSettings
				send.ResendQueueBudget = tx
				send.ResendQueueRetainedByteAccounting = true
				send.ResendQueueMinByteCount = 0
				send.ResendQueueMaxByteCount = leafBytes
				send.WindowSizing = WindowSizingFromDelivery
				send.ApplyWindowSizing()
				send.TransferWireMessageObserver = func(event TransferWireMessageObservation) {
					wireLock.Lock()
					fact := wireFacts[event.MessageId]
					fact.writes++
					fact.sendCount, fact.last, fact.resend = event.SendCount, time.Now(), event.Resend
					var frame protocol.TransferFrame
					if err := ProtoUnmarshal(event.TransferFrameBytes, &frame); err == nil && frame.Pack != nil {
						fact.head = frame.Pack.Head
					}
					wireFacts[event.MessageId] = fact
					wireLock.Unlock()
				}
			})
		defaults := DefaultSendBufferSettings()
		sendSettings := fixture.sender.settings.SendBufferSettings
		if sendSettings.ReliableLaneProvenRecovery || !sendSettings.DeferTimeoutResendWhileCumulativeProgress ||
			!sendSettings.DeferTimeoutResendBackoff || sendSettings.MinResendInterval != defaults.MinResendInterval ||
			sendSettings.RttMinResendInterval != defaults.RttMinResendInterval || sendSettings.MaxResendInterval != defaults.MaxResendInterval ||
			sendSettings.AckTimeout != defaults.AckTimeout || sendSettings.DeliverySizedWindowScale <= 0 {
			t.Fatal("public current-hole fixture changed the ordinary H1 resend or delivery-window policy")
		}
		const prefixCount = 10
		var done [prefixCount + 3]atomic.Bool
		var completed [prefixCount + 3]atomic.Int64
		started := time.Now()
		write := func(slot int) consumerPaidPackByteWire {
			return consumerPaidPackByteSend(t, fixture, 1, 1, slot, &done[slot], &completed[slot], started)
		}
		warm := write(0)
		time.Sleep(25 * time.Millisecond)
		fixture.forward(warm.frame, fixture.receiverIn)
		time.Sleep(25 * time.Millisecond)
		fixture.acknowledge()
		synctest.Wait()
		if !done[0].Load() || tx.UsedByteCount() != 0 {
			t.Fatal("real signed warm original did not complete through its actual cumulative ACK")
		}
		var prefix [prefixCount]consumerPaidPackByteWire
		for index := range prefix {
			prefix[index] = write(index + 1)
		}
		hole := write(prefixCount + 1)
		suffix := write(prefixCount + 2)
		original := hole.frame.pack
		holeId := RequireIdFromBytes(original.MessageId)
		sender := inflightHoleSender(t, fixture, hole.frame)
		if original.Head || original.Nack || original.ContractAhead || original.ContractId != nil || original.ContractFrame != nil ||
			len(original.Frames) != 1 || protocolOnlyPack(original) ||
			!bytes.Equal(original.SequenceId, suffix.frame.pack.SequenceId) ||
			suffix.frame.pack.SequenceNumber != original.SequenceNumber+1 || !sender.stablePacedFifo(sender.sendItems[len(sender.sendItems)-2]) {
			t.Fatal("actual public missing original is not implicit non-Head DATA on the established paced H1 generation")
		}
		// All prefix originals were written before the controlled FIFO delay.
		// Each actual receiver delivery and ACK advances the prefix; no ACK is
		// fabricated, no item timer/clock/RTT estimate is assigned by this test.
		for index, item := range prefix {
			time.Sleep(250 * time.Millisecond)
			fixture.forward(item.frame, fixture.receiverIn)
			fixture.acknowledge()
			synctest.Wait()
			id := RequireIdFromBytes(item.frame.pack.MessageId)
			wireLock.Lock()
			fact := wireFacts[id]
			wireLock.Unlock()
			if !done[index+1].Load() || fact.writes != 1 || fact.sendCount != 1 || fact.resend {
				t.Fatalf("cumulative prefix was not delivered from its sole initial write: index=%d fact=%+v", index, fact)
			}
		}
		synctest.Wait()
		if len(sender.sendItems) != 2 || sender.sendItems[0].messageId != holeId || sender.sendItems[0].sequenceNumber != original.SequenceNumber ||
			sender.sendItems[0].sendCount != 1 || sender.sendItems[0].head || sender.sendItems[0].selectiveAcked ||
			sender.sendItems[0].timeoutDeferCount == 0 || !sender.sendItems[0].deferralOutstanding ||
			done[prefixCount+1].Load() || done[prefixCount+2].Load() {
			t.Fatal("actual first-write cumulative drain did not expose the same deferred undelivered original as its new current head")
		}
		current := time.Now()
		probe = sender.rttWindow.probeRtt(current)
		oldDue = sender.sendItems[0].resendTime.Sub(current)
		deferCount := sender.sendItems[0].timeoutDeferCount
		receiver := consumerForeignHeadSequence(t, fixture, warm.frame)
		if receiver.nextSequenceNumber != original.SequenceNumber || receiver.receiveQueue.Len() != 0 ||
			rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 || tx.UsedByteCount() == 0 ||
			sender.client.SendRecoveryStats().TimeoutResendWriteCount != 0 {
			t.Fatal("current-hole recovery premise changed the actual receiver floor, physical custody or prefix write census")
		}
		wireLock.Lock()
		holeFact := wireFacts[holeId]
		wireLock.Unlock()
		if holeFact.writes != 1 || holeFact.sendCount != 1 || holeFact.head || holeFact.resend {
			t.Fatal("missing original was rewritten before the newly-current transition was measured")
		}
		t.Logf("[current-deferred] number=%d initial_prefix=%d deferrals=%d next_due=%s probe=%s ack_floor=%d lane_rule=false",
			original.SequenceNumber, prefixCount, deferCount, oldDue, probe, receiver.nextSequenceNumber)
		// Observe one ordinary path probe interval. The old code leaves the
		// backed-off timer unchanged; a fix may promote only this exact owner.
		time.Sleep(probe + time.Nanosecond)
		synctest.Wait()
		var retry *windowRoundFrame
		for len(fixture.senderOut) != 0 {
			candidate := fixture.take(fixture.senderOut)
			if candidate.pack == nil || !bytes.Equal(candidate.pack.SequenceId, original.SequenceId) ||
				candidate.pack.SequenceNumber != original.SequenceNumber || !bytes.Equal(candidate.pack.MessageId, original.MessageId) {
				t.Fatal("current transition rewrote an unrelated healthy FIFO owner")
			}
			if retry != nil {
				t.Fatal("current transition published more than one probe of its missing original")
			}
			retry = candidate
		}
		missedProbe = retry == nil
		if retry == nil {
			// Strict old-cause reporting is deferred until the normal immutable
			// sender timer repairs the exact original and all owners are joined.
			retry = pressureContractNaturalRetry(t, fixture, original)
		}
		recoveredAfter = time.Since(current)
		if !retry.pack.Head || !bytes.Equal(retry.pack.SequenceId, original.SequenceId) ||
			!bytes.Equal(retry.pack.MessageId, original.MessageId) || retry.pack.SequenceNumber != original.SequenceNumber ||
			len(retry.pack.Frames) != 1 || !bytes.Equal(retry.pack.Frames[0].MessageBytes, original.Frames[0].MessageBytes) {
			t.Fatal("natural current recovery did not preserve the exact original while becoming a Head")
		}
		fixture.forward(retry, fixture.receiverIn)
		fixture.acknowledge()
		fixture.forward(suffix.frame, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		if !done[prefixCount+1].Load() || !done[prefixCount+2].Load() || len(sender.sendItems) != 0 ||
			receiver.nextSequenceNumber != original.SequenceNumber+2 || root.UsedByteCount() != 0 ||
			len(log.messages()) != 0 {
			t.Fatal("natural retry and real ACKs did not drain the exact missing original, suffix and paid graph")
		}
		t.Logf("[current-deferred-drained] number=%d recovery_after=%s missed_probe=%t", original.SequenceNumber, recoveredAfter, missedProbe)
		fixture.cancel()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("join current deferred recovery Client: %v", err)
			}
		}
		t.Log("[current-deferred-joined]")
	})
	t.Log("[current-deferred-cleanup-complete]")
	if missedProbe {
		t.Errorf("CURRENT_DATA_INHERITS_PREFIX_DEFERRAL: genuine initial-prefix ACKs left the newly-current undelivered original due in %s; natural recovery took %s against the unchanged %s probe interval", oldDue, recoveredAfter, probe)
	}
}
