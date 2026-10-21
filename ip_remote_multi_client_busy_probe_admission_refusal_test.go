// Exercises synchronous local probe admission refusal while established data
// still belongs to the same live Transfer sequence. No production policy is
// changed by this counterfactual fixture.
package connect

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Uses the qualified first-write barrier, with exactly one constructor-owned
// admission slot. The original's serializer still owns that slot after its
// physical write, so subsequent probes cannot acquire it until the barrier
// opens. This changes only test settings, never live admission counters.
func newBusyAdmissionRefusalFixture(t *testing.T) *busyExpiryFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	channelCtx, cancelChannel := context.WithCancel(ctx)
	siblingCtx, cancelSibling := context.WithCancel(ctx)
	destination := NewId()
	firstWritten, releaseFirst := make(chan struct{}), make(chan struct{})
	var firstOnce sync.Once
	observer, events := sendPackLifecycleTestObserver(destination)
	settings := DefaultClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 1
	settings.Log = NewNoopLogger()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
		if id.Destination == destination && number == 0 {
			firstOnce.Do(func() {
				close(firstWritten)
				select {
				case <-releaseFirst:
				case <-ctx.Done():
				}
			})
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	client.ContractManager().AddNoContractPeer(destination)
	route := make(Route, 8)
	client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destination)), []Route{route})
	channel := newPacketTransferTestChannel()
	channel.ctx, channel.cancel = channelCtx, cancelChannel
	channel.client, channel.log = client, NewNoopLogger()
	channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
	channel.settings.CPingWriteTimeout = 20 * time.Millisecond
	sibling := newPacketTransferTestChannel()
	sibling.ctx, sibling.cancel = siblingCtx, cancelSibling
	sibling.log = NewNoopLogger()
	log := &busyExpiryTestLog{Logger: NewNoopLogger()}
	window := busyProbeTestWindow(500*time.Millisecond, channel, sibling)
	window.log = log
	fixture := &busyExpiryFixture{
		client: client, channel: channel, sibling: sibling, window: window,
		destination: destination, route: route, events: events,
		releaseFirst: releaseFirst, log: log,
	}
	t.Cleanup(func() {
		cancel()
		cancelChannel()
		cancelSibling()
		if fixture.verdictDone != nil {
			<-fixture.verdictDone
		}
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("join real busy-probe client: %v", err)
		}
		for len(route) > 0 {
			MessagePoolReturn(<-route)
		}
	})
	packet := MessagePoolGet(1000)
	clear(packet)
	if !channel.SendWithAck(&parsedPacket{packet: packet, ipPath: icmpTcpTestPath(4)}, time.Second, true) {
		MessagePoolReturn(packet)
		t.Fatal("initial real data Pack was not admitted")
	}
	synctest.Wait()
	select {
	case <-firstWritten:
	default:
		t.Fatal("initial data never reached the existing physical-write boundary")
	}
	if len(route) != 1 {
		t.Fatalf("initial route writes=%d, want one real data Pack", len(route))
	}
	wire := <-route
	fixture.firstPack = decodeSendPackLifecycleWirePack(t, wire)
	MessagePoolReturn(wire)
	if fixture.firstPack.Nack || fixture.firstPack.SequenceNumber != 0 {
		t.Fatal("initial data was not the first reliable Pack")
	}
	channel.stateLock.Lock()
	pending, pendingBytes, acked := channel.packetStats.sendNackCount, channel.packetStats.sendNackByteCount, channel.packetStats.sendAckCount
	channel.stateLock.Unlock()
	if pending != 1 || pendingBytes != 1000 || acked != 0 {
		t.Fatalf("real initial flow accounting=%d/%d/%d, want 1/1000/0", pending, pendingBytes, acked)
	}
	return fixture
}

// Two caller-bounded local refusals are two observations of occupied local
// capacity, not two remotely unanswered questions. A fresh receiving sibling
// permits the window's ordinary verdict gates but cannot prove this slot free.
func TestBusyProbeRealLocalAdmissionRefusalsPreserveWrittenFlow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newBusyAdmissionRefusalFixture(t)
		fixture.channel.stateLock.Lock()
		pendingAt := fixture.channel.pendingSendTime
		fixture.channel.stateLock.Unlock()
		started, refused, missingWrite := 0, 0, 0
		convicted := false
		for attempt := range 2 {
			fixture.startVerdict(t)
			time.Sleep(fixture.channel.settings.CPingWriteTimeout)
			synctest.Wait()
			convicted = <-fixture.verdict
			for len(fixture.events) > 0 {
				event := <-fixture.events
				if event.MessageType != protocol.MessageType_IpIpPing {
					continue
				}
				if event.ClientId != fixture.client.ClientId() || event.DestinationId != fixture.destination || !event.AckRequired {
					t.Fatal("probe observation escaped its exact owner")
				}
				if event.Phase == SendPackLifecyclePhaseStarted {
					started++
					continue
				}
				var admission *SendPackAdmissionError
				if !errors.As(event.Err, &admission) || admission.Boundary != "pack-admission" ||
					admission.Timeout != fixture.channel.settings.CPingWriteTimeout || admission.Err != ErrSendPackNotAdmitted {
					t.Fatalf("probe did not fail as exact synchronous false/nil local admission: %+v", event)
				}
				if event.Phase == SendPackLifecyclePhaseFirstRouteWrite {
					missingWrite++
				} else if event.Phase == SendPackLifecyclePhaseTerminal {
					refused++
				} else {
					t.Fatalf("unexpected local admission phase: %v", event.Phase)
				}
			}
			if started != attempt+1 || refused != attempt+1 || missingWrite != attempt+1 ||
				len(fixture.route) != 0 || fixture.client.ReceiveStats().SendPackDeadlineDropCount != 0 {
				t.Fatalf("local admission premise: attempt=%d started=%d refused=%d no-write=%d writes=%d expired=%d",
					attempt, started, refused, missingWrite, len(fixture.route), fixture.client.ReceiveStats().SendPackDeadlineDropCount)
			}
			if attempt == 0 && convicted {
				t.Fatal("first refusal unexpectedly convicted before the two-refusal policy boundary")
			}
		}
		fixture.channel.stateLock.Lock()
		endErr := fixture.channel.endErr
		pending, pendingBytes, acked := fixture.channel.packetStats.sendNackCount, fixture.channel.packetStats.sendNackByteCount, fixture.channel.packetStats.sendAckCount
		clock, probeAck, outstanding := fixture.channel.pendingSendTime, fixture.channel.busyProbeAckTime, fixture.channel.busyProbeOutstanding
		fixture.channel.stateLock.Unlock()
		if convicted || endErr != nil || fixture.channel.ctx.Err() != nil || fixture.client.ctx.Err() != nil {
			t.Fatalf("BUSY_PROBE_LOCAL_ADMISSION_RETIRED_WRITTEN_FLOW convicted=%t end=%v channel=%v client=%v pending=%d refused=%d",
				convicted, endErr, fixture.channel.ctx.Err(), fixture.client.ctx.Err(), pending, refused)
		}
		if pending != 1 || pendingBytes != 1000 || acked != 0 || !clock.Equal(pendingAt) || !probeAck.IsZero() || outstanding {
			t.Fatal("local refusal forged data or probe progress")
		}
		fixture.sibling.stateLock.Lock()
		siblingErr, receives, receivedBytes := fixture.sibling.endErr, fixture.sibling.packetStats.receiveAckCount, fixture.sibling.packetStats.receiveAckByteCount
		fixture.sibling.stateLock.Unlock()
		if siblingErr != nil || fixture.sibling.ctx.Err() != nil || receives != 2 || receivedBytes != 154 {
			t.Fatal("local probe refusal changed the independent receiving sibling")
		}
		close(fixture.releaseFirst)
		synctest.Wait()
		acknowledgeSendPackLifecycleWirePack(t, fixture.client, fixture.destination, fixture.firstPack)
		synctest.Wait()
		packet := MessagePoolGet(1000)
		clear(packet)
		if !fixture.channel.SendWithAck(&parsedPacket{packet: packet, ipPath: icmpTcpTestPath(4)}, time.Second, true) {
			MessagePoolReturn(packet)
			t.Fatal("released local admission did not restore same-channel data")
		}
		synctest.Wait()
		if len(fixture.route) != 1 {
			t.Fatalf("same-channel recovery route writes=%d, want one", len(fixture.route))
		}
		wire := <-fixture.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if pack.Nack || pack.SequenceNumber != 1 || !bytes.Equal(pack.SequenceId, fixture.firstPack.SequenceId) {
			t.Fatal("refused probes changed the surviving reliable sequence")
		}
		acknowledgeSendPackLifecycleWirePack(t, fixture.client, fixture.destination, pack)
		synctest.Wait()
		fixture.channel.stateLock.Lock()
		pending, pendingBytes, acked = fixture.channel.packetStats.sendNackCount, fixture.channel.packetStats.sendNackByteCount, fixture.channel.packetStats.sendAckCount
		fixture.channel.stateLock.Unlock()
		if pending != 0 || pendingBytes != 0 || acked != 2 {
			t.Fatalf("same-sequence recovery accounting=%d/%d/%d, want 0/0/2", pending, pendingBytes, acked)
		}
	})
}
