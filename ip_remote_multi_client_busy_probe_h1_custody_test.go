// Composes real Transfer custody with an actual framed H1 pipe writer/reader.
// Route acceptance, peer receipt, peer ACK and terminal read failure stay distinct.
package connect

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/protocol"
)

// The physical fixture and Client own independent lifetimes. The channel owns
// Client cancellation; its receiving sibling is outside that cancellation tree.
type busyProbeH1Fixture struct {
	client      *Client
	channel     *multiClientChannel
	sibling     *multiClientChannel
	window      *multiClientWindow
	h1          *h1LivenessFixture
	destination Id
	events      <-chan SendPackLifecycleObservation
	firstPack   *protocol.Pack
	startedAt   time.Time
}

// Creates one reliable, peer-received data Pack without acknowledging it. The
// optional peer barrier stops subsequent socket writes, not route admission.
func newBusyProbeH1Fixture(t *testing.T, pausePeer bool, ackTimeout time.Duration) *busyProbeH1Fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	destination := NewId()
	observer, events := sendPackLifecycleTestObserver(destination)
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	settings.SendBufferSettings.AckTimeout = ackTimeout
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	client.ContractManager().AddNoContractPeer(destination)
	h1 := newH1LivenessFixtureWithRoutes(t, pausePeer, 6*time.Second, client.RouteManager())
	channel := newPacketTransferTestChannel()
	channel.settings = DefaultMultiClientSettings()
	channel.settings.CPingWriteTimeout = 20 * time.Millisecond
	channel.settings.BlackholeReceiveTimeout = 0
	channel.settings.BlackholeConnectTimeout = time.Hour
	channel.settings.StatsWindowKeepUnhealthyDuration = time.Second
	channel.ctx, channel.cancel, channel.client, channel.log = ctx, cancel, client, NewNoopLogger()
	channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
	channel.readTimeoutProvider = &ApiMultiClientGenerator{transports: map[*Client]*apiWindowClientTransport{
		client: {settings: h1.transport.settings},
	}}
	sibling := newPacketTransferTestChannel()
	sibling.ctx, sibling.cancel = context.WithCancel(context.Background())
	sibling.log = NewNoopLogger()
	window := busyProbeTestWindow(500*time.Millisecond, channel, sibling)
	window.log = NewNoopLogger()
	fixture := &busyProbeH1Fixture{
		client: client, channel: channel, sibling: sibling, window: window,
		h1: h1, destination: destination, events: events, startedAt: time.Now(),
	}
	t.Cleanup(func() {
		channel.Cancel()
		sibling.cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	packet := MessagePoolGet(1000)
	clear(packet)
	path := icmpTcpTestPath(4)
	path.Syn = false
	if !channel.SendWithAck(&parsedPacket{packet: packet, ipPath: path}, time.Second, true) {
		MessagePoolReturn(packet)
		t.Fatal("initial data was refused before the physical H1 owner")
	}
	synctest.Wait()
	messages := h1.payloadMessagesSnapshot()
	if len(messages) != 1 || h1.client.writesInProgress.Load() != 0 {
		t.Fatal("first data Pack was not actually consumed by the H1 peer")
	}
	fixture.firstPack = decodeSendPackLifecycleWirePack(t, messages[0])
	if fixture.firstPack.Nack || fixture.firstPack.SequenceNumber != 0 {
		t.Fatal("initial peer receipt was not the reliable sequence head")
	}
	if pausePeer {
		select {
		case <-h1.peerReadBlocked:
		default:
			t.Fatal("peer reader did not park at its complete-message barrier")
		}
	}
	return fixture
}

// Returns the actual probe lifecycle identity after production admission. It
// deliberately does not equate the first-route receipt with a socket write.
func (self *busyProbeH1Fixture) startProbe(t *testing.T) (<-chan bool, SendPackLifecycleObservation) {
	t.Helper()
	time.Sleep(30 * time.Millisecond)
	self.sibling.addReceiveAck(77)
	finished := make(chan bool, 1)
	go func() { finished <- self.window.convictSendStalls(20 * time.Millisecond) }()
	synctest.Wait()
	var started SendPackLifecycleObservation
	firstRoute := false
	for len(self.events) > 0 {
		event := <-self.events
		if event.MessageType != protocol.MessageType_IpIpPing {
			continue
		}
		if event.Phase == SendPackLifecyclePhaseStarted {
			started = event
		} else if event.Phase == SendPackLifecyclePhaseFirstRouteWrite && event.Err == nil {
			firstRoute = true
		} else {
			t.Fatalf("probe took an unexpected admission disposition: %+v", event)
		}
	}
	if started.Token == 0 || !firstRoute || !started.AckRequired || self.channel.busyProbeSendFunc != nil || !self.channel.busyProbeOutstandingNow() {
		t.Fatal("probe did not use production reliable admission on the actual H1 route")
	}
	return finished, started
}

// The peer writes a cumulative ACK through the actual framed socket reader.
// The marshaled buffer stays owned here; WriteMessage only borrows it.
func (self *busyProbeH1Fixture) ackFromPeer(t *testing.T, pack *protocol.Pack) {
	t.Helper()
	wire, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: sendTransferPath(self.destination, DestinationId(self.client.ClientId())).ToProtobuf(),
		Ack:          &protocol.Ack{MessageId: append([]byte(nil), pack.MessageId...), SequenceId: append([]byte(nil), pack.SequenceId...)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(wire)
	if err := self.h1.peer.WriteMessage(websocket.BinaryMessage, wire); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
}

// A route-accepted probe can be blocked in the actual H1 writer while inbound
// heartbeats keep that same carrier healthy. Neither boundary proves peer silence.
func TestBusyProbeH1HeldWriterKeepsHealthyResidentFlows(t *testing.T) {
	resetH1UpgradeTestState(t)
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newBusyProbeH1Fixture(t, true, 20*time.Second)
		finished, probe := fixture.startProbe(t)
		if fixture.h1.client.writesInProgress.Load() != 1 || fixture.h1.payloads.Load() != 1 {
			t.Fatal("route-accepted probe did not remain behind real socket backpressure")
		}
		fixture.channel.stateLock.Lock()
		pendingAt := fixture.channel.pendingSendTime
		fixture.channel.stateLock.Unlock()
		time.Sleep(500 * time.Millisecond)
		sendH1BackpressureHeartbeat(t, fixture.h1)
		if <-finished || fixture.channel.IsDone() || fixture.client.ctx.Err() != nil || fixture.sibling.IsDone() {
			t.Fatal("bounded admission timer retired the healthy backpressured H1 owner")
		}
		fixture.h1.assertConnected(t)
		for range 4 {
			fixture.sibling.addReceiveAck(77)
			if fixture.window.convictSendStalls(20 * time.Millisecond) {
				t.Fatal("repeated evaluation convicted the same unresolved physical owner")
			}
		}
		if len(fixture.events) != 0 || fixture.h1.payloads.Load() != 1 || fixture.h1.client.writesInProgress.Load() != 1 {
			t.Fatal("repeated held passes accumulated another probe or invented physical progress")
		}
		fixture.channel.stateLock.Lock()
		unchanged := fixture.channel.pendingSendTime.Equal(pendingAt) && fixture.channel.packetStats.sendNackCount == 1 &&
			fixture.channel.packetStats.sendNackByteCount == 1000 && fixture.channel.packetStats.sendAckCount == 0 &&
			fixture.channel.busyProbeAckTime.IsZero() && !fixture.channel.busyProbeOutstanding
		fixture.channel.stateLock.Unlock()
		if !unchanged {
			t.Fatal("holding a physical writer forged an ACK or kept a suspect penalty")
		}
		close(fixture.h1.peerReadResume)
		synctest.Wait()
		messages := fixture.h1.payloadMessagesSnapshot()
		if len(messages) != 2 || fixture.h1.client.writesInProgress.Load() != 0 {
			t.Fatal("releasing the peer did not finish the exact queued probe")
		}
		probePack := decodeSendPackLifecycleWirePack(t, messages[1])
		if probePack.SequenceNumber != 1 || !bytes.Equal(probePack.SequenceId, fixture.firstPack.SequenceId) {
			t.Fatal("H1 recovery replaced or bypassed the original reliable sequence")
		}
		fixture.ackFromPeer(t, probePack)
		terminal := 0
		for len(fixture.events) > 0 {
			event := <-fixture.events
			if event.Token == probe.Token {
				if event.Phase != SendPackLifecyclePhaseTerminal || event.Err != nil {
					t.Fatal("physical peer ACK lost exact probe completion custody")
				}
				terminal++
			}
		}
		fixture.channel.stateLock.Lock()
		pending, pendingBytes, acked := fixture.channel.packetStats.sendNackCount, fixture.channel.packetStats.sendNackByteCount, fixture.channel.packetStats.sendAckCount
		fixture.channel.stateLock.Unlock()
		if terminal != 1 || pending != 0 || pendingBytes != 0 || acked != 1 || fixture.sibling.IsDone() {
			t.Fatalf("real H1 recovery terminal=%d data=%d/%d/%d", terminal, pending, pendingBytes, acked)
		}
		fixture.h1.assertConnected(t)
	})
}

// Actual socket read silence still withdraws the carrier at its configured
// deadline. The existing empty-carrier health owner then retires the channel;
// the short busy-probe timer neither preempts nor extends either lifetime.
func TestBusyProbeH1ReadTimeoutStillRetiresSilentChannel(t *testing.T) {
	resetH1UpgradeTestState(t)
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newBusyProbeH1Fixture(t, false, 20*time.Second)
		healthDone := make(chan struct{})
		go func() {
			defer close(healthDone)
			fixture.channel.detectBlackhole()
		}()
		t.Cleanup(func() { fixture.channel.Cancel(); <-healthDone })
		synctest.Wait()
		finished, probe := fixture.startProbe(t)
		messages := fixture.h1.payloadMessagesSnapshot()
		if len(messages) != 2 || fixture.h1.client.writesInProgress.Load() != 0 || fixture.channel.noSendAckTimeout() != 3*time.Second {
			t.Fatal("negative control did not physically deliver its unanswered probe")
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if <-finished || fixture.channel.IsDone() {
			t.Fatal("local probe budget preempted the independent read authority")
		}
		readAt := fixture.startedAt.Add(fixture.h1.transport.settings.ReadTimeout)
		time.Sleep(time.Until(readAt) - time.Nanosecond)
		synctest.Wait()
		fixture.h1.assertConnected(t)
		if fixture.channel.IsDone() {
			t.Fatal("silent channel retired before the actual read deadline")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		fixture.h1.assertWithdrawn(t)
		if fixture.h1.client.writeTimedOut.Load() {
			t.Fatal("read-silence control accidentally expired a writer")
		}
		// Health observes down at 3.75s, then the existing 1s migration bound
		// matures at its next 1.25s poll. These are fixture settings/cadence.
		time.Sleep(time.Until(fixture.startedAt.Add(5 * time.Second)))
		synctest.Wait()
		select {
		case <-healthDone:
		default:
			t.Fatal("silent carrier lost its bounded channel-retirement owner")
		}
		fixture.channel.stateLock.Lock()
		err, owner := fixture.channel.endErr, fixture.channel.busyProbe
		fixture.channel.stateLock.Unlock()
		if !fixture.channel.IsDone() || fixture.client.ctx.Err() == nil || fixture.sibling.IsDone() || err != errTransportDownTimeout || owner != nil {
			t.Fatalf("independent read/down authority failed: channel=%v client=%v error=%v owner=%t", fixture.channel.ctx.Err(), fixture.client.ctx.Err(), err, owner != nil)
		}
		terminals := 0
		for len(fixture.events) > 0 {
			event := <-fixture.events
			if event.Token == probe.Token && event.Phase == SendPackLifecyclePhaseTerminal {
				if event.Err == nil {
					t.Fatal("silent probe received a fabricated terminal ACK")
				}
				terminals++
			}
		}
		if terminals != 1 || !strings.Contains(err.Error(), "transport") {
			t.Fatalf("silent probe terminal custody=%d error=%v", terminals, err)
		}
	})
}

// Canceling logical probe custody does not need a socket-write timeout. The
// independent physical owner is then joined explicitly and returns its roots.
func TestBusyProbeH1CancellationJoinsWithoutProbeTimeout(t *testing.T) {
	resetH1UpgradeTestState(t)
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newBusyProbeH1Fixture(t, true, 20*time.Second)
		finished, _ := fixture.startProbe(t)
		if fixture.h1.client.writesInProgress.Load() != 1 {
			t.Fatal("cancellation control never reached actual H1 write pressure")
		}
		at := time.Now()
		fixture.channel.Cancel()
		if err := fixture.client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if <-finished || time.Now() != at || fixture.sibling.IsDone() || fixture.channel.busyProbe != nil || fixture.channel.busyProbeOutstandingNow() {
			t.Fatal("logical cancellation waited for the probe, retained custody, or canceled its sibling")
		}
		fixture.h1.assertConnected(t)
		if err := fixture.h1.transport.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		fixture.h1.assertWithdrawn(t)
		if time.Now() != at || fixture.h1.client.writesInProgress.Load() != 0 || fixture.h1.client.writeTimedOut.Load() {
			t.Fatal("physical cancellation waited for a timeout or left its writer alive")
		}
	})
}
