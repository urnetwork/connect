package connect

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A real singleton TCP admission expires while its SendSequence has not run.
// No Transfer sequence number, contract debit, or physical write exists for
// this packet. The inner TCP may retry; this local refusal must not remove the
// provider, reset its other flows, or claim a peer ACK. Virtual time crosses
// the original caller deadline; no callback error or Pack deadline is injected.
func TestQueuedReliableIpExpiryDoesNotPoisonProvider(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startSequence := make(chan struct{})
		observer, events := sendPackLifecycleTestObserver(destination)
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.SendPackLifecycleObserver = observer
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == destination {
				select {
				case <-startSequence:
				case <-ctx.Done():
				}
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 4)
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("client cleanup: %v", err)
			}
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(
			NewSendClientTransport(DestinationId(destination)), []Route{route},
		)
		channel := newPacketTransferTestChannel()
		channel.client = client
		channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
		const unrelatedBytes ByteCount = 1440
		channel.addSend(unrelatedBytes, icmpTcpTestPath(4))
		send := func() {
			t.Helper()
			packet := MessagePoolGet(1000)
			clear(packet)
			admitted, err := channel.SendDetailedWithAck(&parsedPacket{
				packet: packet, ipPath: icmpTcpTestPath(4),
			}, time.Second, true)
			if !admitted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("TCP admission=%t, %v", admitted, err)
			}
		}
		send()
		synctest.Wait()
		time.Sleep(2 * time.Second)
		close(startSequence)
		synctest.Wait()
		if len(route) != 0 {
			t.Fatal("expired Pack reached the route")
		}
		if got := client.ReceiveStats().SendPackDeadlineDropCount; got != 1 {
			t.Fatalf("deadline drops=%d, want 1", got)
		}
		if len(events) != 3 {
			t.Fatalf("expired Pack lifecycle phases=%d, want 3", len(events))
		}
		started, first, terminal := <-events, <-events, <-events
		if started.Phase != SendPackLifecyclePhaseStarted ||
			first.Phase != SendPackLifecyclePhaseFirstRouteWrite ||
			terminal.Phase != SendPackLifecyclePhaseTerminal ||
			!terminal.AckRequired || terminal.MessageType != protocol.MessageType_IpIpPacketToProvider ||
			!errors.Is(terminal.Err, ErrSendPackNotAdmitted) {
			t.Fatalf("unexpected expiry lifecycle: %+v / %+v / %+v", started, first, terminal)
		}
		if _, err := channel.WindowStats(); err != nil {
			t.Fatalf("unwritten TCP expiry poisoned the provider: %v", err)
		}
		channel.stateLock.Lock()
		pending, pendingBytes := channel.packetStats.sendNackCount, channel.packetStats.sendNackByteCount
		acked := channel.packetStats.sendAckCount
		channel.stateLock.Unlock()
		if pending != 1 || pendingBytes != unrelatedBytes || acked != 0 {
			t.Fatalf("pending packets/bytes/ACK credit=%d/%d/%d, want 1/%d/0", pending, pendingBytes, acked, unrelatedBytes)
		}
		if _, _, measured, _ := channel.rttEwmaSnapshot(); measured {
			t.Fatal("local expiry fabricated an ACK RTT")
		}

		// The retry must use the same live sequence, with its first sequence
		// number still available, and complete only on a real cumulative ACK.
		send()
		synctest.Wait()
		if len(route) != 1 {
			t.Fatalf("retry route writes=%d, want 1", len(route))
		}
		wire := <-route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if pack.SequenceNumber != 0 || pack.Nack {
			t.Fatalf("retry sequence number/NoAck=%d/%t, want 0/false", pack.SequenceNumber, pack.Nack)
		}
		acknowledgeSendPackLifecycleWirePack(t, client, destination, pack)
		synctest.Wait()
		channel.stateLock.Lock()
		pending, pendingBytes = channel.packetStats.sendNackCount, channel.packetStats.sendNackByteCount
		acked = channel.packetStats.sendAckCount
		channel.stateLock.Unlock()
		if pending != 1 || pendingBytes != unrelatedBytes || acked != 1 {
			t.Fatalf("retry pending packets/bytes/ACK credit=%d/%d/%d", pending, pendingBytes, acked)
		}
		channel.addSendAck(unrelatedBytes)
	})
}

// ErrSendPackNotAdmitted can also come from a retained-frame head rewrite.
// Only the explicit unwritten marker is exculpatory for an ACK-required IP
// packet; joining that marker with any ambiguous or structural error remains
// fatal. This pins the same boundary for singleton and grouped callbacks.
func TestReliableIpUnwrittenExpiryKeepsStructuralFailures(t *testing.T) {
	for _, row := range []struct {
		name  string
		err   error
		local bool
	}{
		{"unwritten", errSendPackExpiredUnwritten, true},
		{"wrapped-unwritten", fmt.Errorf("queued: %w", errSendPackExpiredUnwritten), true},
		{"joined-unwritten", errors.Join(errSendPackExpiredUnwritten, errSendPackExpiredUnwritten), true},
		{"ambiguous-admission", ErrSendPackNotAdmitted, false},
		{"write-timeout", errTransferRouteWriteTimeout, false},
		{"joined-ambiguous", errors.Join(errSendPackExpiredUnwritten, ErrSendPackNotAdmitted), false},
		{"joined-timeout", errors.Join(errSendPackExpiredUnwritten, errTransferRouteWriteTimeout), false},
		{"joined-sequence-close", errors.Join(errSendPackExpiredUnwritten, errors.New("Send sequence closed.")), false},
		{"wrapped-structural", fmt.Errorf("group: %w", errors.Join(errSendPackExpiredUnwritten, context.Canceled)), false},
	} {
		for _, grouped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/group=%t", row.name, grouped), func(t *testing.T) {
				channel := newPacketTransferTestChannel()
				if grouped {
					group := newPacketTransferTestGroup()
					channel.addSendGroup(group)
					channel.observePacketGroupTransferCompletion(group, true, row.err)
				} else {
					channel.addSend(1000, icmpTcpTestPath(4))
					channel.observePacketTransferCompletion(1000, time.Now(), true, row.err)
				}
				if _, err := channel.WindowStats(); (err == nil) != row.local {
					t.Fatalf("completion=%v, want packet-local=%t", err, row.local)
				}
				if row.local {
					channel.stateLock.Lock()
					pending, acked := channel.packetStats.sendNackCount, channel.packetStats.sendAckCount
					channel.stateLock.Unlock()
					if pending != 0 || acked != 0 {
						t.Fatalf("local refusal retained pending/ACK=%d/%d", pending, acked)
					}
				}
			})
		}
	}
}
