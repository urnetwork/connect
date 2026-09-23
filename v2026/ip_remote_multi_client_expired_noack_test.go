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
	for _, grouped := range []bool{false, true} {
		t.Run(fmt.Sprintf("grouped=%t", grouped), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			harness := newNoAckFastPathHarness(t, ctx, 0)
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
			originalCallback := queued.AckCallback
			completed := make(chan error, 1)
			queued.AckCallback = func(err error) {
				originalCallback(err)
				completed <- err
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
				}
			}()
			select {
			case err := <-completed:
				if !errors.Is(err, ErrSendPackNotAdmitted) {
					t.Fatalf("deadline disposition=%v", err)
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
