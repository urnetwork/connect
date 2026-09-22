package connect

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The optional idle ping uses a control-message callback, not the ordinary
// packet classifier. Exercise that actual SendDetailedMessage/SendSequence
// path: its queued ping expires locally, without any wire attempt, while an
// unrelated TCP flow is still active on the same provider.
func TestCpingQueuedUnwrittenExpiryLeavesChannelAlive(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		channelCtx, cancelChannel := context.WithCancel(ctx)
		defer cancelChannel()
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
		pingDone := make(chan struct{})
		defer func() {
			cancel()
			<-pingDone
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("client cleanup: %v", err)
			}
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destination)), []Route{route})
		channel := newPacketTransferTestChannel()
		channel.ctx, channel.cancel = channelCtx, cancelChannel
		channel.client = client
		channel.log = NewNoopLogger()
		channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
		channel.settings.CPingMaxByteCountPerSecond = 0
		channel.settings.CPingWriteTimeout = time.Second
		channel.settings.CPingTimeout = time.Minute
		const pendingBytes ByteCount = 1440
		channel.addSend(pendingBytes, icmpTcpTestPath(4))
		go func() {
			defer close(pingDone)
			channel.ping()
		}()
		synctest.Wait()
		if len(events) != 1 {
			t.Fatalf("ping admission lifecycle phases=%d, want 1", len(events))
		}
		started := <-events
		if started.MessageType != protocol.MessageType_IpIpPing || !started.AckRequired {
			t.Fatalf("not a reliable control ping: %+v", started)
		}
		// This is the original CPingWriteTimeout, not a mutated Pack deadline.
		time.Sleep(2 * time.Second)
		close(startSequence)
		synctest.Wait()
		select {
		case <-pingDone:
		default:
			t.Fatal("expired ping did not end its optional monitoring loop")
		}
		if len(route) != 0 || client.ReceiveStats().SendPackDeadlineDropCount != 1 {
			t.Fatal("ping did not take the pre-wire queue-expiry branch")
		}
		if len(events) != 2 {
			t.Fatalf("ping expiry lifecycle phases=%d, want 2", len(events))
		}
		first, terminal := <-events, <-events
		if first.Phase != SendPackLifecyclePhaseFirstRouteWrite ||
			terminal.Phase != SendPackLifecyclePhaseTerminal ||
			terminal.MessageType != protocol.MessageType_IpIpPing ||
			!errors.Is(terminal.Err, errSendPackExpiredUnwritten) {
			t.Fatalf("wrong expiry result: %+v / %+v", first, terminal)
		}
		if channelCtx.Err() != nil {
			t.Fatal("a locally expired control ping canceled the shared provider")
		}
		if _, err := channel.WindowStats(); err != nil {
			t.Fatalf("a locally expired control ping poisoned the provider: %v", err)
		}
		channel.stateLock.Lock()
		pending, bytes, acked := channel.packetStats.sendNackCount, channel.packetStats.sendNackByteCount, channel.packetStats.sendAckCount
		channel.stateLock.Unlock()
		if pending != 1 || bytes != pendingBytes || acked != 0 {
			t.Fatalf("ping changed TCP pending/bytes/ACK accounting: %d/%d/%d", pending, bytes, acked)
		}

		// The surviving sequence can still write and acknowledge real data.
		packet := MessagePoolGet(1000)
		clear(packet)
		if !channel.SendWithAck(&parsedPacket{packet: packet, ipPath: icmpTcpTestPath(4)}, time.Second, true) {
			MessagePoolReturn(packet)
			t.Fatal("TCP send refused after ping expiry")
		}
		synctest.Wait()
		if len(route) != 1 {
			t.Fatalf("TCP route writes=%d, want 1", len(route))
		}
		wire := <-route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if pack.Nack || pack.SequenceNumber != 0 {
			t.Fatalf("TCP reliability/sequence changed: NoAck=%t sequence=%d", pack.Nack, pack.SequenceNumber)
		}
		acknowledgeSendPackLifecycleWirePack(t, client, destination, pack)
		synctest.Wait()
		channel.stateLock.Lock()
		pending, bytes, acked = channel.packetStats.sendNackCount, channel.packetStats.sendNackByteCount, channel.packetStats.sendAckCount
		channel.stateLock.Unlock()
		if pending != 1 || bytes != pendingBytes || acked != 1 {
			t.Fatalf("TCP completion accounting=%d/%d/%d", pending, bytes, acked)
		}
		channel.addSendAck(pendingBytes)
	})
}

// The exemption requires the exact local-expiry proof. An actual Transfer
// failure, ambiguous admission error, or mixed error group still retires the
// channel. A wrapper must not lose the proof, and a join must not hide failure.
func TestCpingUnwrittenExpiryKeepsStructuralErrorsFatal(t *testing.T) {
	for _, row := range []struct {
		name  string
		err   error
		local bool
	}{
		{"marked", errSendPackExpiredUnwritten, true},
		{"wrapped-marked", fmt.Errorf("ping: %w", errSendPackExpiredUnwritten), true},
		{"joined-marked", errors.Join(errSendPackExpiredUnwritten, errSendPackExpiredUnwritten), true},
		{"generic-admission", ErrSendPackNotAdmitted, false},
		{"write-timeout", errTransferRouteWriteTimeout, false},
		{"sequence-close", errors.New("Send sequence closed."), false},
		{"mixed-admission", errors.Join(errSendPackExpiredUnwritten, ErrSendPackNotAdmitted), false},
		{"mixed-cancellation", errors.Join(errSendPackExpiredUnwritten, context.Canceled), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			channel := newPacketTransferTestChannel()
			channel.ctx, channel.cancel = ctx, cancel
			channel.log = NewNoopLogger()
			channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(NewId())}
			channel.settings.CPingTimeout = time.Second
			channel.pingSendForTest = func(_ time.Duration, callback func(error)) (bool, error) {
				callback(row.err)
				return true, nil
			}
			channel.ping()
			_, err := channel.WindowStats()
			if (err == nil) != row.local || (ctx.Err() == nil) != row.local {
				t.Fatalf("error=%v canceled=%v, want packet-local=%t", err, ctx.Err(), row.local)
			}
		})
	}
}
