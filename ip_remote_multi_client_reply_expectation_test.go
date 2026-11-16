// Native controls and application payload use real paired Transfer clients.
// Only the remote application's response is absent; Pack ACKs are never forged.
package connect

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The peer callback retains only bounded scalar evidence of its actual input.
type replyExpectationPacket struct {
	destination netip.Addr
	ack         bool
	rst         bool
	syn         bool
	fin         bool
	payload     bool
	bytes       int
}

// The existing group producer owns the source. A separate real receiver owns
// delivery and its ordinary cumulative Pack ACKs over the return route.
type replyExpectationFixture struct {
	base        *groupDispositionQueueFixture
	peer        *Client
	stateLock   sync.Mutex
	packets     [16]replyExpectationPacket
	updates     [8]*multiClientChannelUpdate
	packetCount int
	invalid     bool
	holdAcks    atomic.Bool
	heldAck     atomic.Bool
}

// The return gate retains one real ACK and drains later ACKs without blocking
// the peer writer. Cancellation releases and joins it before source witnesses.
func newReplyExpectationFixture(t *testing.T) *replyExpectationFixture {
	t.Helper()
	self := &replyExpectationFixture{}
	self.base = newGroupDispositionQueueFixture(t, nil, nil)
	self.base.selected.settings = self.base.parent.settings
	// The original fixture's single-flow seam must not lend sequence coverage
	// across these distinct native tuples. The source set is fixed and finite.
	self.base.parent.sendClientPathForTest = func(path *IpPath, _ flowPin, callback func(*multiClientChannelUpdate, *multiClientChannel)) {
		index := path.SourcePort - 45000
		if index < 0 || len(self.updates) <= index || path.Version != 4 || path.Protocol != IpProtocolTcp ||
			!path.DestinationIp.Equal(net.IPv4(198, 51, 100, byte(90+index%2))) {
			t.Fatal("group source escaped its fixed tuple owners")
		}
		self.stateLock.Lock()
		update := self.updates[index]
		if update == nil {
			update = newMultiClientChannelUpdate(self.base.parent.ctx, path)
			update.client.Store(self.base.selected)
			self.updates[index] = update
		}
		self.stateLock.Unlock()
		callback(update, update.client.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	settings := closeWaitClientSettings()
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() { <-ctx.Done() }
	self.peer = NewClient(ctx, self.base.peer, NewNoContractClientOob(), settings)
	self.peer.ContractManager().AddNoContractPeer(self.base.client.ClientId())
	peerOut, sourceIn := make(Route, 32), make(Route, 32)
	self.peer.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{self.base.route})
	self.peer.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(self.base.client.ClientId())), []Route{peerOut})
	self.base.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{sourceIn})
	self.peer.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
				continue
			}
			packet, err := ipPacketToProviderBytes(frame)
			if err != nil {
				t.Error("peer could not decode its actual native frame: ", err)
				continue
			}
			path, payload, err := ParseIpPathWithPayload(packet)
			if err != nil || source.SourceId != self.base.client.ClientId() || path.Version != 4 || path.Protocol != IpProtocolTcp {
				t.Error("peer received an unexpected source or native packet")
				continue
			}
			destination, ok := netip.AddrFromSlice(path.DestinationIp)
			self.stateLock.Lock()
			if !ok || self.packetCount == len(self.packets) || len(payload) != 0 && !bytes.Equal(payload, []byte("native request awaits a response")) {
				self.invalid = true
			} else {
				self.packets[self.packetCount] = replyExpectationPacket{
					destination: destination.Unmap(), ack: path.Ack, rst: path.Rst,
					syn: path.Syn, fin: path.Fin, payload: len(payload) != 0, bytes: len(packet),
				}
				self.packetCount++
			}
			self.stateLock.Unlock()
		}
		// Returning lets this real ReceiveSequence produce its normal Pack ACK.
		// The remote application deliberately sends no native packet back.
	})
	var relay sync.WaitGroup
	relay.Add(1)
	go func() {
		defer relay.Done()
		var held []byte
		defer func() {
			if held != nil {
				MessagePoolReturn(held)
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case wire := <-peerOut:
				if self.holdAcks.Load() {
					if !replyExpectationIsAck(wire) {
						self.stateLock.Lock()
						self.invalid = true
						self.stateLock.Unlock()
						MessagePoolReturn(wire)
					} else if held == nil {
						held = wire
						self.heldAck.Store(true)
					} else {
						// Suppress later genuine ACKs without occupying the route.
						MessagePoolReturn(wire)
					}
					continue
				}
				select {
				case sourceIn <- wire:
				case <-ctx.Done():
					MessagePoolReturn(wire)
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		for _, update := range self.updates {
			if update != nil {
				update.Close()
			}
		}
		self.base.cancel()
		self.base.unpark()
		cancel()
		if err := self.base.client.CloseAndWait(context.Background()); err != nil {
			t.Error("join real source: ", err)
		}
		if err := self.peer.CloseAndWait(context.Background()); err != nil {
			t.Error("join real peer: ", err)
		}
		relay.Wait()
		for _, route := range []Route{peerOut, sourceIn} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
	})
	self.base.unpark()
	return self
}

// Only a decoded peer-produced acknowledgement can attest the held return.
// Both supported wire layouts are read without manufacturing a receipt.
func replyExpectationIsAck(wire []byte) bool {
	var transferFrame protocol.TransferFrame
	if err := ProtoUnmarshal(wire, &transferFrame); err != nil {
		return false
	}
	ack := transferFrame.Ack
	if ack == nil {
		frame := transferFrame.GetFrame()
		if frame == nil || frame.MessageType != protocol.MessageType_TransferAck {
			return false
		}
		ack = &protocol.Ack{}
		if err := ProtoUnmarshal(frame.MessageBytes, ack); err != nil {
			return false
		}
	}
	return len(ack.MessageId) == len(Id{}) && len(ack.SequenceId) == len(Id{}) && len(ack.MissingContractId) == 0
}

// Both destinations and the source are reserved documentation addresses.
func replyExpectationPath(destination int, sourcePort int) *IpPath {
	return &IpPath{Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.IPv4(192, 0, 2, 91), SourcePort: sourcePort,
		DestinationIp: net.IPv4(198, 51, 100, byte(90+destination)), DestinationPort: 443,
		SequenceNumber: uint32(100000 + sourcePort)}
}

// The grouped native entry owns each ACK/data original. RSTs use the actual
// generated-teardown producer and its independent flow/source enrollment.
func (self *replyExpectationFixture) offer(t *testing.T, kind string, round int) {
	t.Helper()
	for destination := 0; destination < 2; destination++ {
		path := replyExpectationPath(destination, 45000+2*round+destination)
		if kind == "rst" {
			packet, ok := ipOosRst(path)
			if !ok {
				t.Fatal("TCP teardown did not produce its native reset")
			}
			owned := MessagePoolCopy(packet)
			self.base.witnesses = append(self.base.witnesses, MessagePoolShareReadOnly(owned))
			update := newMultiClientChannelUpdate(self.base.parent.ctx, path)
			update.client.Store(self.base.selected)
			t.Cleanup(update.Close)
			accepted := update.sendAffinityControl(self.base.selected, owned, 0)
			MessagePoolReturn(owned)
			if !accepted {
				t.Fatal("actual teardown producer refused its reset")
			}
			continue
		}
		var payload []byte
		flags := byte(tcpFlagAck)
		if kind == "payload" {
			payload = []byte("native request awaits a response")
			flags |= tcpFlagPsh
		}
		packet := reraceTcpPacket(path, flags, path.SequenceNumber, 7001, payload)
		if self.base.offer(packet) != 1 {
			t.Fatal("native group producer refused its original")
		}
	}
}

// Only actual receiver delivery and source completion satisfy this barrier.
// The bound contains ordinary default ACK compression; no timer is changed.
func (self *replyExpectationFixture) await(t *testing.T, received, acknowledged, pending int) *clientWindowStats {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		synctest.Wait()
		self.stateLock.Lock()
		count, invalid := self.packetCount, self.invalid
		self.stateLock.Unlock()
		stats, err := self.base.selected.WindowStats()
		if err != nil || invalid {
			t.Fatalf("real carrier failed its premise: err=%v invalid=%t", err, invalid)
		}
		if count == received && stats.sendAckCount == acknowledged && stats.sendNackCount == pending {
			return stats
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("real peer/source did not join receipt: received=%d/%d acknowledged=%d/%d pending=%d/%d", count, received, stats.sendAckCount, acknowledged, stats.sendNackCount, pending)
		}
		time.Sleep(defaultAckCompressTimeout)
	}
}

// Three real send/receipt rounds make complete historical buckets without
// editing clocks, counters or lifetime evidence. All health settings are defaults.
func testNativeReplyExpectation(t *testing.T, kind string, holdPayloadAcks bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newReplyExpectationFixture(t)
		settings := fixture.base.selected.settings
		defaults := DefaultMultiClientSettings()
		if settings.BlackholeReceiveTimeout != defaults.BlackholeReceiveTimeout || settings.BlackholeTimeout != defaults.BlackholeTimeout ||
			settings.BlackholeConnectTimeout != defaults.BlackholeConnectTimeout || settings.StatsWindowDuration != defaults.StatsWindowDuration ||
			settings.StatsWindowBucketDuration != defaults.StatsWindowBucketDuration || settings.MinBlackholeDestinations != defaults.MinBlackholeDestinations {
			t.Fatal("carrier fixture changed default health policy")
		}
		acknowledged, received, pending := 0, 0, 0
		if holdPayloadAcks {
			// First earn real peer liveness. Held DATA cannot borrow a fabricated
			// ACK or have its outstanding custody erased by earlier controls.
			fixture.offer(t, "ack", 0)
			received, acknowledged = 2, 2
			fixture.await(t, received, acknowledged, 0)
			fixture.holdAcks.Store(true)
		}
		firstOffer := time.Now()
		for round := 0; round < 3; round++ {
			if round != 0 {
				time.Sleep(settings.StatsWindowBucketDuration + time.Nanosecond)
			}
			fixture.offer(t, kind, round+1)
			received += 2
			if holdPayloadAcks {
				pending += 2
			} else {
				acknowledged += 2
			}
			fixture.await(t, received, acknowledged, pending)
		}
		// Rolling receive evidence matures inside its original 30s window.
		// The held-DATA control instead proves the independent lifetime debt
		// survives after those earlier protocol receipts have aged out.
		maturityDelay := settings.BlackholeReceiveTimeout + settings.StatsWindowBucketDuration
		want := blackholeNone
		if kind == "payload" {
			want = blackholeNoReceiveAck
		}
		if holdPayloadAcks {
			maturityDelay = settings.BlackholeTimeout + settings.StatsWindowBucketDuration
			want = blackholeNoSendAck
			acknowledged = 0
		}
		maturity := firstOffer.Add(maturityDelay)
		if delay := time.Until(maturity); 0 < delay {
			time.Sleep(delay)
		}
		stats := fixture.await(t, received, acknowledged, pending)
		if !holdPayloadAcks && (time.Since(firstOffer) >= settings.StatsWindowDuration || !hasPeerSendAckInWindow(stats)) ||
			holdPayloadAcks && (time.Since(firstOffer) < settings.BlackholeTimeout || hasPeerSendAckInWindow(stats)) ||
			stats.receiveAckCount != 0 || stats.receiveSynCount != 0 || stats.sendSynCount != 0 ||
			fixture.base.client.IsDone() || fixture.peer.IsDone() || !fixture.base.selected.hasActiveTransport() {
			t.Fatal("mature snapshot lost expected receipt, silence, or carrier lifetime")
		}
		fixture.stateLock.Lock()
		packets, count := fixture.packets, fixture.packetCount
		fixture.stateLock.Unlock()
		var destinations [2]bool
		var deliveredPayloadBytes ByteCount
		for index, packet := range packets[:count] {
			wantPayload := kind == "payload" && (!holdPayloadAcks || 2 <= index)
			if packet.syn || packet.fin || packet.payload != wantPayload ||
				packet.rst != (kind == "rst") || packet.ack != (kind != "rst") {
				t.Fatalf("actual peer packet class differs from the controlled source: kind=%s packet=%+v", kind, packet)
			}
			if wantPayload {
				deliveredPayloadBytes += ByteCount(packet.bytes)
			}
			for destination := range destinations {
				if packet.destination == netip.AddrFrom4([4]byte{198, 51, 100, byte(90 + destination)}) {
					destinations[destination] = true
				}
			}
		}
		if destinations != [2]bool{true, true} || count != received ||
			holdPayloadAcks && (!fixture.heldAck.Load() || stats.sendNackByteCount != deliveredPayloadBytes || deliveredPayloadBytes == 0) {
			t.Fatal("real two-destination delivery or held DATA ownership was not proved")
		}
		// This isolates the ordinary admissible-evidence branch. The bare
		// channel fixture does not claim a separate live sibling uplink.
		reason, held := blackholeReasonFromStats(time.Now(), stats,
			settings.BlackholeTimeout, settings.BlackholeReceiveTimeout, settings.BlackholeConnectTimeout,
			blackholeGates{minReceiveAckDestinations: settings.MinBlackholeDestinations})
		if reason != want || held != blackholeNone {
			t.Fatalf("NATIVE_CONTROL_REPLY_EXPECTATION: kind=%s held_payload_acks=%t peer_packets=%d peer_acked=%d pending=%d/%d destinations=%d receive=%d/%d verdict=%s held=%s want=%s",
				kind, holdPayloadAcks, count, stats.sendAckCount, stats.sendNackCount, stats.sendNackByteCount,
				stats.sendDestinationCount, stats.receiveAckCount, stats.receiveSynCount, reason, held, want)
		}
	})
}

// Acknowledging native TCP feedback does not promise another native packet.
func TestNativeAckOnlyPeerReceiptsDoNotCreateReceiveSilence(t *testing.T) {
	testNativeReplyExpectation(t, "ack", false)
}

// The actual generated-reset producer must not convict its own drained exit.
func TestNativeTeardownRstPeerReceiptsDoNotCreateReceiveSilence(t *testing.T) {
	testNativeReplyExpectation(t, "rst", false)
}

// A real payload accepted by the peer still expects native return progress.
func TestNativePayloadPeerReceiptsKeepReceiveSilenceEvidence(t *testing.T) {
	testNativeReplyExpectation(t, "payload", false)
}

// Earlier protocol receipts must not erase a distinct retained DATA owner.
func TestNativePendingPayloadKeepsSendSilenceEvidence(t *testing.T) {
	testNativeReplyExpectation(t, "payload", true)
}
