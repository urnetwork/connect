// Exercises local pre-wire probe expiry through real Transfer ownership and
// distinguishes exact local expiry from route admission without peer delivery.
package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Collects bounded decision lines without changing the tested client logger.
type busyExpiryTestLog struct {
	Logger
	stateLock sync.Mutex
	lines     []string
}

func (self *busyExpiryTestLog) Infof(format string, args ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, fmt.Sprintf(format, args...))
}

func (self *busyExpiryTestLog) snapshot() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.lines, "\n")
}

// Owns a real written-but-unacknowledged data Pack. Only its sequence owner
// can be held, after the first physical route write and before dequeuing the
// next Pack. A subsequently queued probe expires at its original deadline.
type busyExpiryFixture struct {
	client       *Client
	channel      *multiClientChannel
	sibling      *multiClientChannel
	window       *multiClientWindow
	destination  Id
	route        Route
	events       <-chan SendPackLifecycleObservation
	firstPack    *protocol.Pack
	releaseFirst chan struct{}
	verdictDone  chan struct{}
	verdict      chan bool
	log          *busyExpiryTestLog
}

func newBusyExpiryFixture(t *testing.T, holdFirst bool) *busyExpiryFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	channelCtx, cancelChannel := context.WithCancel(ctx)
	siblingCtx, cancelSibling := context.WithCancel(ctx)
	destination := NewId()
	firstWritten, releaseFirst := make(chan struct{}), make(chan struct{})
	var firstOnce sync.Once
	observer, events := sendPackLifecycleTestObserver(destination)
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
		if id.Destination == destination && number == 0 {
			firstOnce.Do(func() {
				close(firstWritten)
				if holdFirst {
					select {
					case <-releaseFirst:
					case <-ctx.Done():
					}
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

func (self *busyExpiryFixture) startVerdict(t *testing.T) {
	t.Helper()
	time.Sleep(30 * time.Millisecond)
	self.sibling.addReceiveAck(77)
	if !self.channel.sendStalled(20*time.Millisecond) || !self.sibling.hasRecentReceive(520*time.Millisecond) {
		t.Fatal("actual outstanding data/sibling evidence did not admit the busy probe")
	}
	self.verdict, self.verdictDone = make(chan bool, 1), make(chan struct{})
	go func() {
		defer close(self.verdictDone)
		self.verdict <- self.window.convictSendStalls(20 * time.Millisecond)
	}()
	synctest.Wait()
}

// Reproduces the field event through the real queued Pack expiry and the
// real window verdict, with a different data Pack already physically written.
func TestBusyProbeRealQueuedExpiryPreservesWrittenFlow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newBusyExpiryFixture(t, true)
		fixture.channel.stateLock.Lock()
		pendingAt := fixture.channel.pendingSendTime
		fixture.channel.stateLock.Unlock()
		fixture.startVerdict(t)
		var probeStarted SendPackLifecycleObservation
		for len(fixture.events) > 0 {
			event := <-fixture.events
			if event.MessageType == protocol.MessageType_IpIpPing && event.Phase == SendPackLifecyclePhaseStarted {
				probeStarted = event
			}
		}
		if probeStarted.Token == 0 || !probeStarted.AckRequired || len(fixture.route) != 0 {
			t.Fatal("probe did not queue behind the already-written data owner")
		}
		time.Sleep(30 * time.Millisecond)
		close(fixture.releaseFirst)
		synctest.Wait()
		var terminal SendPackLifecycleObservation
		var firstWrite SendPackLifecycleObservation
		for len(fixture.events) > 0 {
			event := <-fixture.events
			if event.Token != probeStarted.Token {
				continue
			}
			if event.Phase == SendPackLifecyclePhaseFirstRouteWrite {
				firstWrite = event
			}
			if event.Phase == SendPackLifecyclePhaseTerminal {
				terminal = event
			}
		}
		if !packetTransferExpiredUnwritten(firstWrite.Err) || !packetTransferExpiredUnwritten(terminal.Err) ||
			firstWrite.Token != probeStarted.Token || terminal.Token != probeStarted.Token ||
			fixture.client.ReceiveStats().SendPackDeadlineDropCount != 1 || len(fixture.route) != 0 {
			t.Fatalf("real local expiry premise failed: first=%+v terminal=%+v drops=%d writes=%d", firstWrite, terminal, fixture.client.ReceiveStats().SendPackDeadlineDropCount, len(fixture.route))
		}
		convicted := <-fixture.verdict
		fixture.channel.stateLock.Lock()
		endErr, pending, pendingBytes, acked := fixture.channel.endErr, fixture.channel.packetStats.sendNackCount, fixture.channel.packetStats.sendNackByteCount, fixture.channel.packetStats.sendAckCount
		clock, probeAck, outstanding := fixture.channel.pendingSendTime, fixture.channel.busyProbeAckTime, fixture.channel.busyProbeOutstanding
		fixture.channel.stateLock.Unlock()
		if convicted || endErr != nil || fixture.channel.ctx.Err() != nil || fixture.client.ctx.Err() != nil {
			t.Fatalf("BUSY_PROBE_LOCAL_EXPIRY_RETIRED_WRITTEN_FLOW convicted=%t end=%v channel=%v client=%v pending=%d", convicted, endErr, fixture.channel.ctx.Err(), fixture.client.ctx.Err(), pending)
		}
		if pending != 1 || pendingBytes != 1000 || acked != 0 || !clock.Equal(pendingAt) || !probeAck.IsZero() || outstanding {
			t.Fatalf("local probe forged flow progress: pending=%d bytes=%d acked=%d clock=%v probeAck=%v outstanding=%t", pending, pendingBytes, acked, clock, probeAck, outstanding)
		}
		fixture.sibling.stateLock.Lock()
		siblingErr, receives, receivedBytes := fixture.sibling.endErr, fixture.sibling.packetStats.receiveAckCount, fixture.sibling.packetStats.receiveAckByteCount
		fixture.sibling.stateLock.Unlock()
		if siblingErr != nil || fixture.sibling.ctx.Err() != nil || receives != 1 || receivedBytes != 77 {
			t.Fatal("local probe changed the independent receiving sibling")
		}
		acknowledgeSendPackLifecycleWirePack(t, fixture.client, fixture.destination, fixture.firstPack)
		synctest.Wait()
		packet := MessagePoolGet(1000)
		clear(packet)
		if !fixture.channel.SendWithAck(&parsedPacket{packet: packet, ipPath: icmpTcpTestPath(4)}, time.Second, true) {
			MessagePoolReturn(packet)
			t.Fatal("same channel refused real data after the local probe expired")
		}
		synctest.Wait()
		if len(fixture.route) != 1 {
			t.Fatalf("post-expiry route writes=%d, want one", len(fixture.route))
		}
		wire := <-fixture.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if pack.Nack || pack.SequenceNumber != fixture.firstPack.SequenceNumber+1 || !bytes.Equal(pack.SequenceId, fixture.firstPack.SequenceId) {
			t.Fatal("local expiry replaced the surviving reliable sequence")
		}
		acknowledgeSendPackLifecycleWirePack(t, fixture.client, fixture.destination, pack)
		synctest.Wait()
		fixture.channel.stateLock.Lock()
		pending, pendingBytes, acked = fixture.channel.packetStats.sendNackCount, fixture.channel.packetStats.sendNackByteCount, fixture.channel.packetStats.sendAckCount
		fixture.channel.stateLock.Unlock()
		if pending != 0 || pendingBytes != 0 || acked != 2 {
			t.Fatalf("real data ACK accounting after recovery=%d/%d/%d, want 0/0/2", pending, pendingBytes, acked)
		}
	})
}

// Supersedes the old route-only "written silence" control. A nil first-route
// receipt means channel acceptance, not physical socket completion or peer ACK.
func TestBusyProbeRouteAdmissionDoesNotProveWireSilence(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newBusyExpiryFixture(t, false)
		fixture.startVerdict(t)
		if len(fixture.route) != 1 {
			t.Fatalf("real probe route writes=%d, want one", len(fixture.route))
		}
		wire := <-fixture.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if pack.Nack || pack.SequenceNumber != 1 || !bytes.Equal(pack.SequenceId, fixture.firstPack.SequenceId) {
			t.Fatal("remote-negative probe did not really use the same reliable route")
		}
		probeWritten := false
		for len(fixture.events) > 0 {
			event := <-fixture.events
			if event.MessageType == protocol.MessageType_IpIpPing && event.Phase == SendPackLifecyclePhaseFirstRouteWrite && event.Err == nil {
				probeWritten = true
			}
		}
		if !probeWritten || fixture.client.ReceiveStats().SendPackDeadlineDropCount != 0 {
			t.Fatal("remote-negative probe accidentally took local expiry")
		}
		time.Sleep(501 * time.Millisecond)
		synctest.Wait()
		if <-fixture.verdict || fixture.channel.ctx.Err() != nil || fixture.client.ctx.Err() != nil {
			t.Fatal("route-channel admission was mistaken for physical peer silence")
		}
		fixture.channel.stateLock.Lock()
		endErr := fixture.channel.endErr
		fixture.channel.stateLock.Unlock()
		if endErr != nil || fixture.channel.busyProbeOutstandingNow() {
			t.Fatalf("unknown route-only observation retained an error or tier penalty: %v", endErr)
		}
		if fixture.sibling.ctx.Err() != nil {
			t.Fatal("remote conviction canceled the independently receiving sibling")
		}
	})
}

// Local expiry has no ACK and cannot refresh the stale episode merely to
// bound logging. The existing episode latch emits one held line until real
// data ACK progress ends the episode; it then permits one new held line.
func TestBusyProbeLocalExpiryHeldLogIsEpisodeBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel := busyProbeTestChannel(t, func(_ time.Duration, callback func(error)) (bool, error) {
			callback(errSendPackExpiredUnwritten)
			return true, nil
		})
		channel.addSend(1440, icmpTcpTestPath(4))
		time.Sleep(30 * time.Millisecond)
		sibling := receivingSibling()
		window := busyProbeTestWindow(500*time.Millisecond, channel, sibling)
		log := &busyExpiryTestLog{Logger: NewNoopLogger()}
		window.log = log
		channel.stateLock.Lock()
		pendingAt := channel.pendingSendTime
		channel.stateLock.Unlock()
		for range 3 {
			if window.convictSendStalls(20 * time.Millisecond) {
				t.Fatal("repeated exact local expiry became a remote conviction")
			}
		}
		if got := log.snapshot(); strings.Count(got, "outcome=held") != 1 || strings.Contains(got, "outcome=acquitted") || strings.Contains(got, "outcome=convicted") {
			t.Fatalf("local-expiry narration was not one held record: %s", got)
		}
		channel.stateLock.Lock()
		clock, ack, pending, acked := channel.pendingSendTime, channel.busyProbeAckTime, channel.packetStats.sendNackCount, channel.packetStats.sendAckCount
		channel.stateLock.Unlock()
		if !clock.Equal(pendingAt) || !ack.IsZero() || pending != 1 || acked != 0 {
			t.Fatal("bounded logging forged an ACK or reset the pending flow")
		}
		channel.addSendAck(1440)
		if channel.sendStalled(20 * time.Millisecond) {
			t.Fatal("real completed data did not close the stale episode")
		}
		channel.addSend(1440, icmpTcpTestPath(4))
		time.Sleep(30 * time.Millisecond)
		sibling.addReceiveAck(77)
		if window.convictSendStalls(20*time.Millisecond) || strings.Count(log.snapshot(), "outcome=held") != 2 {
			t.Fatal("new real pending episode did not get exactly one local held record")
		}
		channel.addSendAck(1440)
	})
}

// Cancellation and a failed callback can become ready together. Neither select
// arm may turn channel teardown into a peer failure or a successful peer ACK.
func TestBusyProbeCanceledCallbackDoesNotJudgePeer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cancel context.CancelFunc
		channel := busyProbeTestChannel(t, func(_ time.Duration, callback func(error)) (bool, error) {
			cancel()
			callback(errors.New("synthetic canceled sequence failure"))
			return true, nil
		})
		cancel = channel.cancel
		stallPast(channel, 20*time.Millisecond)
		channel.stateLock.Lock()
		pendingAt := channel.pendingSendTime
		pendingBefore := channel.packetStats.sendNackCount
		channel.stateLock.Unlock()
		sibling := receivingSibling()
		sibling.ctx = context.Background()
		window := busyProbeTestWindow(500*time.Millisecond, channel, sibling)
		log := &busyExpiryTestLog{Logger: NewNoopLogger()}
		window.log = log
		if window.convictSendStalls(20 * time.Millisecond) {
			t.Fatal("canceled callback convicted a closing channel")
		}
		channel.stateLock.Lock()
		endErr, ack := channel.endErr, channel.busyProbeAckTime
		pendingAfter, clock := channel.packetStats.sendNackCount, channel.pendingSendTime
		outstanding := channel.busyProbeOutstanding
		channel.stateLock.Unlock()
		if channel.ctx.Err() != context.Canceled || endErr != nil || !ack.IsZero() || outstanding || pendingBefore != pendingAfter || !clock.Equal(pendingAt) {
			t.Fatalf("cancellation changed peer-health evidence: context=%v end=%v ack=%v outstanding=%t pending=%d/%d", channel.ctx.Err(), endErr, ack, outstanding, pendingBefore, pendingAfter)
		}
		if sibling.IsDone() || log.snapshot() != "" {
			t.Fatal("canceled callback changed its sibling or narrated a peer-health result")
		}
	})
}
