package connect

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Drive the actual SendSequence deadline branch, including a logical group.
// A queued UDP expiry is a local refusal and must not retire the channel that
// also owns an outstanding TCP send. No callback error is injected here.
func TestQueuedNoAckExpiryKeepsSharedTcpChannel(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, grouped := range []bool{false, true} {
		t.Run(fmt.Sprintf("grouped=%t", grouped), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			harness := newNoAckFastPathHarness(t, ctx, 0)
			t.Cleanup(func() { closeTransferGroupTestClient(t, harness.client) })
			channel := newPacketTransferTestChannel()
			channel.client = harness.client
			channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(harness.destinationId)}
			const tcpBytes ByteCount = 1440
			channel.addSend(tcpBytes, icmpTcpTestPath(4))
			var admitted bool
			var err error
			if grouped {
				admitted, err = channel.SendGroupDetailedWithAck(newPacketTransferTestGroup(), time.Hour, false)
			} else {
				admitted, err = channel.SendDetailedWithAck(&parsedPacket{
					packet: make([]byte, 1000), ipPath: udpTestPath(4),
				}, time.Hour, false)
			}
			if !admitted || err != nil {
				t.Fatalf("initial queue admission=%t, %v", admitted, err)
			}
			queued := <-harness.sequence.packs
			if queued.Ack || queued.logicalGroup != grouped {
				t.Fatalf("queued packet Ack/grouped=%t/%t", queued.Ack, queued.logicalGroup)
			}
			// The sequence has not started; move the actual queued Pack past
			// its deadline to pin the expiry branch without waiting or racing.
			queued.deadline = time.Now().Add(-time.Second)
			if queued.AckCallback == nil && queued.ackTarget == nil {
				t.Fatal("queued packet has no completion owner")
			}
			// Terminal follows either the real callback or the typed group target.
			// Leave both untouched; wrapping only AckCallback misses target-backed sends.
			originalObserver := queued.lifecycleObserver
			type expiryCompletion struct {
				err          error
				pendingCount int
				pendingBytes ByteCount
				ackedCount   int
			}
			completed := make(chan expiryCompletion, 1)
			terminalCount := 0
			terminalOverflow := false
			queued.lifecycleObserver = func(observation SendPackLifecycleObservation) {
				if originalObserver != nil {
					originalObserver(observation)
				}
				if observation.Phase != SendPackLifecyclePhaseTerminal {
					return
				}
				channel.stateLock.Lock()
				completion := expiryCompletion{
					err:          observation.Err,
					pendingCount: channel.packetStats.sendNackCount,
					pendingBytes: channel.packetStats.sendNackByteCount,
					ackedCount:   channel.packetStats.sendAckCount,
				}
				channel.stateLock.Unlock()
				terminalCount++
				select {
				case completed <- completion:
				default:
					terminalOverflow = true
				}
			}
			budget := harness.sequence.resendQueue.budget
			var budgetBefore TransferMemoryBudgetStats
			if budget != nil {
				budgetBefore = budget.Stats()
			}
			harness.sequence.packs <- queued
			done := make(chan struct{})
			go func() {
				defer close(done)
				harness.sequence.Run()
			}()
			defer func() {
				harness.sequence.cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("expired-packet sequence did not join")
					return
				}
				// Run owns these fields until its actual join, including pool return.
				if terminalCount != 1 || terminalOverflow {
					t.Errorf("terminal observations=%d overflow=%t, want one", terminalCount, terminalOverflow)
				}
				sequence := harness.sequence
				if count, bytes := sequence.resendQueue.QueueSize(); count != 0 || bytes != 0 {
					t.Errorf("expired packet retained queue owners=%d/%d", count, bytes)
				}
				if len(sequence.packs) != 0 || len(sequence.ackLifetimes.items) != 0 ||
					sequence.currentPreparedHandoff != nil || sequence.writeCount.Load() != 0 {
					t.Error("expired packet retained admission/lifetime/prepared ownership or reached a writer")
				}
				for _, item := range sequence.sendItems {
					if item != nil {
						t.Error("expired packet retained a send item after Run joined")
					}
				}
				if admission := sequence.packAdmission; admission != nil {
					admission.mutex.Lock()
					count, keys := admission.count, len(admission.byKey)
					admission.mutex.Unlock()
					if count != 0 || keys != 0 {
						t.Errorf("expired packet retained admission credits=%d keys=%d", count, keys)
					}
				}
				if budget != nil {
					if after := budget.Stats(); after.UsedByteCount != budgetBefore.UsedByteCount ||
						after.ReservedByteCount != budgetBefore.ReservedByteCount ||
						after.ReleasedByteCount != budgetBefore.ReleasedByteCount {
						t.Errorf("unserialized expiry changed retained credit: before=%+v after=%+v", budgetBefore, after)
					}
				}
			}()
			select {
			case completion := <-completed:
				err := completion.err
				if !errors.Is(err, ErrSendPackNotAdmitted) {
					t.Fatalf("deadline disposition=%v", err)
				}
				if completion.pendingCount != 1 || completion.pendingBytes != tcpBytes || completion.ackedCount != 0 {
					t.Fatalf("terminal preceded callback/target accounting: pending=%d/%d ACK=%d",
						completion.pendingCount, completion.pendingBytes, completion.ackedCount)
				}
			case <-time.After(time.Second):
				t.Fatal("queued datagram never reached deadline disposition")
			}
			if _, err := channel.WindowStats(); err != nil {
				t.Fatalf("local NoAck expiry poisoned the shared provider: %v", err)
			}
			channel.stateLock.Lock()
			pendingCount, pendingBytes := channel.packetStats.sendNackCount, channel.packetStats.sendNackByteCount
			ackedCount := channel.packetStats.sendAckCount
			channel.stateLock.Unlock()
			if pendingCount != 1 || pendingBytes != tcpBytes || ackedCount != 0 {
				t.Fatalf("pending packets/bytes/ACK credit=%d/%d/%d, want 1/%d/0", pendingCount, pendingBytes, ackedCount, tcpBytes)
			}
			if _, _, rttOK, _ := channel.rttEwmaSnapshot(); rttOK {
				t.Fatal("local expiry fabricated RTT evidence")
			}
			channel.addSendAck(tcpBytes)
			if channel.sendStalled(time.Nanosecond) {
				t.Fatal("expired datagram left false send-stall evidence after TCP completion")
			}
			stats := harness.client.ReceiveStats()
			if stats.SendPackDeadlineDropCount != 1 || stats.SendNoAckDiscardCount != 1 || stats.SendNoAckWriteCount != 0 {
				t.Fatalf("deadline branch was not isolated: %+v", stats)
			}
		})
	}
}

// Chunk completion uses errors.Join. Every child must be packet-local before
// a whole NoAck group can be abandoned; one structural failure stays fatal.
func TestPacketTransferLocalRefusalPreservesStructuralFailures(t *testing.T) {
	structural := errors.New("Send sequence closed.")
	for _, row := range []struct {
		name  string
		err   error
		local bool
	}{
		{"expiry", ErrSendPackNotAdmitted, true},
		{"wrapped-expiry", fmt.Errorf("queued: %w", ErrSendPackNotAdmitted), true},
		{"joined-local", errors.Join(ErrSendPackNotAdmitted, errTransferRouteWriteTimeout), true},
		{"wrapped-local-group", fmt.Errorf("group: %w", errors.Join(ErrSendPackNotAdmitted, errTransferRouteWriteTimeout)), true},
		{"joined-structural", errors.Join(ErrSendPackNotAdmitted, structural), false},
		{"joined-timeout-structural", errors.Join(errTransferRouteWriteTimeout, structural), false},
		{"wrapped-structural-group", fmt.Errorf("group: %w", errors.Join(ErrSendPackNotAdmitted, structural)), false},
	} {
		for _, grouped := range []bool{false, true} {
			for _, ack := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/group=%t/ack=%t", row.name, grouped, ack), func(t *testing.T) {
					channel := newPacketTransferTestChannel()
					if grouped {
						group := newPacketTransferTestGroup()
						channel.addSendGroup(group)
						channel.observePacketGroupTransferCompletion(group, ack, row.err)
					} else {
						channel.addSend(1000, udpTestPath(4))
						channel.observePacketTransferCompletion(1000, time.Time{}, ack, row.err)
					}
					_, err := channel.WindowStats()
					if wantFailure := ack || !row.local; (err != nil) != wantFailure {
						t.Fatalf("completion=%v, want provider failure=%t", err, wantFailure)
					}
				})
			}
		}
	}
}
