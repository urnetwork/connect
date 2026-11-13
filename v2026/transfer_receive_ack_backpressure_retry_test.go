package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// An ACK which never reached its route is not a lost network ACK: the local
// writer knows no carrier owns it. Preserve that feedback until a bounded
// route becomes writable, without requiring the peer to retransmit its data.
// The production 15-second write and 30-second sender lifetimes are unchanged.
// This is a deterministic mechanism test, not proof that every live PERFVAR
// ACK timeout has this cause.
func TestReceiveSequenceAckRecoversAfterBoundedReplyBackpressure(t *testing.T) {
	for _, blockedFor := range []time.Duration{14 * time.Second, 16 * time.Second} {
		t.Run(blockedFor.String(), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				fixture := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
					settings.AckCompressTimeout = 10 * time.Millisecond
					settings.WriteTimeout = 15 * time.Second
				})
				for range cap(fixture.route) {
					fixture.route <- nil
				}
				messageID := NewId()
				fixture.receiveSequence.sendAck(181, messageID, false,
					sequenceTag{}, false, TransportTypeP2p)
				synctest.Wait()
				time.Sleep(blockedFor)
				synctest.Wait()
				errorCount := fixture.receiveSequence.client.receiveAckRouteWriteErrorCount.Load()
				if blockedFor > 15*time.Second && errorCount != 1 {
					t.Fatalf("expected one local ACK write expiry, got %d", errorCount)
				}
				// Capacity returns before the unchanged peer lifetime. No new
				// Pack or ACK-window update is injected to rescue the old ACK.
				for range cap(fixture.route) {
					if wire := <-fixture.route; wire != nil {
						MessagePoolReturn(wire)
						t.Fatal("blocked route unexpectedly accepted the ACK")
					}
				}
				got, ok := fixture.readAck(t, time.Second)
				if !ok || got != messageID {
					t.Fatal("locally unwritten cumulative ACK was discarded after bounded backpressure; peer would require a retransmit")
				}
			})
		})
	}
}

func TestReceiveSequenceAckRetryCoalescesNewHeadAndRetainsMetadata(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		opened, release := make(chan struct{}), make(chan struct{})
		fixture := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.WriteTimeout = 15 * time.Second
			settings.EvictionNotice = true
			settings.afterAckWriterOpenForTest = func(receiveSequenceId, MultiRouteWriter) {
				close(opened)
				<-release
			}
		})
		<-opened
		for range cap(fixture.route) {
			fixture.route <- nil
		}
		sequence := fixture.receiveSequence
		newHead, selective, missing, contract := NewId(), NewId(), NewId(), NewId()
		sequence.noteEviction(200)
		sequence.sendAck(181, NewId(), false, sequenceTag{}, false, TransportTypeP2p)
		sequence.sendAck(183, selective, true, sequenceTag{}, false, TransportTypeP2p)
		sequence.ackWindow.UpdateContractMissing(sequenceAck{
			sequenceNumber: 184, messageId: missing, contractMissing: true,
			missingContractId: contract, transportType: TransportTypeP2p,
		})
		close(release)
		synctest.Wait()
		time.Sleep(15 * time.Second)
		synctest.Wait()
		sequence.sendAck(182, newHead, false, sequenceTag{}, false, TransportTypeP2p)
		time.Sleep(time.Second)
		synctest.Wait()
		for range cap(fixture.route) {
			if wire := <-fixture.route; wire != nil {
				MessagePoolReturn(wire)
				t.Fatal("blocked route unexpectedly accepted feedback")
			}
		}
		head := readBurstTailTestAck(t, fixture)
		if RequireIdFromBytes(head.MessageId) != newHead || head.Selective ||
			len(head.EvictedSequenceNumbers) != 1 || head.EvictedSequenceNumbers[0] != 200 {
			t.Fatal("retry lost eviction metadata or failed to absorb old cumulative head")
		}
		sack := readBurstTailTestAck(t, fixture)
		if RequireIdFromBytes(sack.MessageId) != selective || !sack.Selective {
			t.Fatal("retry lost above-head selective feedback")
		}
		request := readBurstTailTestAck(t, fixture)
		if RequireIdFromBytes(request.MessageId) != missing ||
			RequireIdFromBytes(request.MissingContractId) != contract {
			t.Fatal("retry lost missing-contract feedback")
		}
	})
}

func TestReceiveSequenceAckRetryPreservesOnlyUnwrittenResponseSuffix(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		opened, release := make(chan struct{}), make(chan struct{})
		fixture := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.WriteTimeout = 15 * time.Second
			settings.EvictionNotice = true
			settings.afterAckWriterOpenForTest = func(receiveSequenceId, MultiRouteWriter) {
				close(opened)
				<-release
			}
		})
		<-opened
		// Only the first frame fits. Its eviction metadata must not be
		// repeated when the second frame is restored after its local timeout.
		for range cap(fixture.route) - 1 {
			fixture.route <- nil
		}
		headID, selectiveID := NewId(), NewId()
		fixture.receiveSequence.noteEviction(200)
		fixture.receiveSequence.sendAck(181, headID, false, sequenceTag{}, false, TransportTypeP2p)
		fixture.receiveSequence.sendAck(183, selectiveID, true, sequenceTag{}, false, TransportTypeP2p)
		close(release)
		synctest.Wait()
		time.Sleep(16 * time.Second)
		synctest.Wait()
		for range cap(fixture.route) - 1 {
			if wire := <-fixture.route; wire != nil {
				MessagePoolReturn(wire)
				t.Fatal("ACK overtook a prefilled route entry")
			}
		}
		head := readBurstTailTestAck(t, fixture)
		if RequireIdFromBytes(head.MessageId) != headID || head.Selective ||
			len(head.EvictedSequenceNumbers) != 1 || head.EvictedSequenceNumbers[0] != 200 {
			t.Fatal("initial successful head lost its eviction metadata")
		}
		selective := readBurstTailTestAck(t, fixture)
		if RequireIdFromBytes(selective.MessageId) != selectiveID || !selective.Selective ||
			len(selective.EvictedSequenceNumbers) != 0 {
			t.Fatal("retry replayed a successful ACK or its metadata instead of only the unwritten suffix")
		}
		if _, ok := fixture.readAck(t, 20*time.Millisecond); ok {
			t.Fatal("retry duplicated previously successful response feedback")
		}
	})
}

func TestReceiveSequenceAckRetryZeroWaitIsBoundedAndCancelable(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 0
			settings.WriteTimeout = 0
		})
		for range cap(fixture.route) {
			fixture.route <- nil
		}
		fixture.receiveSequence.sendAck(181, NewId(), false, sequenceTag{}, false, TransportTypeP2p)
		synctest.Wait()
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		errors := fixture.receiveSequence.client.receiveAckRouteWriteErrorCount.Load()
		if errors < 2 || errors > 11 {
			t.Fatalf("zero-wait ACK retry attempts=%d, want bounded 2..11 in 10ms", errors)
		}
		fixture.receiveSequence.Cancel()
		synctest.Wait()
		select {
		case <-fixture.receiveSequence.exit:
		default:
			t.Fatal("retry retained a canceled ACK worker")
		}
	})
}

func TestReceiveSequenceAckRetryDoesNotRequeueClosedWriter(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 0
			settings.WriteTimeout = 15 * time.Second
			settings.afterAckWriterOpenForTest = func(_ receiveSequenceId, writer MultiRouteWriter) {
				writer.(*MultiRouteSelector).cancel()
			}
		})
		for range cap(fixture.route) {
			fixture.route <- nil
		}
		fixture.receiveSequence.sendAck(181, NewId(), false, sequenceTag{}, false, TransportTypeP2p)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		if errors := fixture.receiveSequence.client.receiveAckRouteWriteErrorCount.Load(); errors != 1 {
			t.Fatalf("closed writer was retried %d times", errors)
		}
		if fixture.receiveSequence.ackWindow.Pending() {
			t.Fatal("structurally closed writer retained pending ACK work")
		}
	})
}

// Retrying an old response must not overwrite a later tag/receiver timestamp
// which arrived while that response was blocked. Plaintext/negotiated evidence
// still has to survive the merge, including a missing-contract request.
func TestSequenceAckRestorePreservesNewerExactFeedback(t *testing.T) {
	for _, missing := range []bool{false, true} {
		window := newSequenceAckWindow()
		id := NewId()
		old := sequenceAck{
			sequenceNumber: 183, messageId: id, selective: !missing, contractMissing: missing,
			tag: sequenceTag{sendTime: 111, set: true}, receivedAtNanos: 100,
			unwrapped: true, compactContractRecoverySupported: true,
			transportType: TransportTypeP2p, missingContractId: NewId(),
		}
		newer := old
		newer.tag.sendTime, newer.receivedAtNanos = 222, 200
		newer.unwrapped, newer.compactContractRecoverySupported = false, false
		if missing {
			window.UpdateContractMissing(old)
		} else {
			window.Update(old)
		}
		response, _ := window.takeResponse(make([]sequenceAck, 0, ackResponseMaxCount), ackResponseMaxCount)
		if missing {
			window.UpdateContractMissing(newer)
		} else {
			window.Update(newer)
		}
		window.Restore(response)
		got, _ := window.takeResponse(response[:0], ackResponseMaxCount)
		if len(got) != 1 || got[0].messageId != id || got[0].tag.sendTime != 222 ||
			got[0].receivedAtNanos != 200 || !got[0].unwrapped || !got[0].compactContractRecoverySupported {
			t.Fatal("unwritten response restoration overwrote newer exact feedback")
		}
	}
}

func TestSequenceAckRestoreHeadDoesNotAllocate(t *testing.T) {
	window := newSequenceAckWindow()
	ack := sequenceAck{messageId: NewId()}
	var scratch [ackResponseMaxCount]sequenceAck
	allocations := testing.AllocsPerRun(1000, func() {
		ack.sequenceNumber++
		window.Update(ack)
		acks, _ := window.takeResponse(scratch[:0], len(scratch))
		window.Restore(acks)
		acks, overflow := window.takeResponse(scratch[:0], len(scratch))
		if len(acks) != 1 || overflow {
			t.Fatal("restored cumulative ACK did not coalesce to one head")
		}
	})
	if allocations != 0 {
		t.Fatalf("head ACK retry allocated %.1f objects", allocations)
	}
}
