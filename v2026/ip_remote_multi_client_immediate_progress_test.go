// Control packets delivered ahead of a receive gap prove an exit is reachable,
// not that its return stream moves. Real Transfer Clients carry them to the
// exit channel's own receive callback; only the exit state is a bare fixture.
package connect

import (
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A provider stand-in sends to the exit's real receive Client. One DATA Pack
// is held back to open a young gap; the exit channel sees only what the real
// ReceiveSequence hands its callback.
type immediateProgressFixture struct {
	t           *testing.T
	wire        *windowRoundFixture
	returnPath  *IpPath
	controlPath *IpPath
	missing     *windowRoundFrame
	base        uint64
}

// Must run inside a synctest bubble. The warm original is not an IP packet,
// so the exit has no receive evidence when the gap opens.
func newImmediateProgressFixture(t *testing.T, exit *multiClientChannel) *immediateProgressFixture {
	t.Helper()
	wire := newWindowRoundFixture(t, nil, nil)
	exit.ctx = wire.ctx
	wire.receiver.AddReceiveCallback(exit.clientReceive)
	fixture := &immediateProgressFixture{
		t:    t,
		wire: wire,
		returnPath: &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.ParseIP("198.51.100.40").To4(), SourcePort: 443,
			DestinationIp: net.ParseIP("10.44.0.2").To4(), DestinationPort: 44201},
		controlPath: &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.ParseIP("198.51.100.41").To4(), SourcePort: 443,
			DestinationIp: net.ParseIP("10.44.0.2").To4(), DestinationPort: 44202},
	}
	warm := wire.write(16)
	wire.forward(warm, wire.receiverIn)
	wire.acknowledge()
	dataSequence := uint32(1000)
	wire.frameFactory = func(payloadByteCount int) *protocol.Frame {
		packet := MessagePoolCopy(ipOosTcpPacketSequence(fixture.returnPath, tcpFlagAck|tcpFlagPsh,
			dataSequence, make([]byte, payloadByteCount)))
		dataSequence += uint32(payloadByteCount)
		frame, err := ipPacketFromProviderFrame(packet, 2)
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	fixture.missing = wire.write(1240)
	fixture.base = fixture.missing.pack.SequenceNumber
	if receiver := wire.receiveSequence(); receiver.nextSequenceNumber != fixture.base {
		t.Fatal("the missing DATA is not at the exit receiver's floor")
	}
	return fixture
}

// Sends one control packet as the provider's return path does and delivers
// its wire Pack to the exit Client.
func (self *immediateProgressFixture) sendControl(packet []byte, immediate bool) *windowRoundFrame {
	self.t.Helper()
	owned := MessagePoolCopy(packet)
	frame, err := ipPacketFromProviderFrame(owned, 2)
	if err != nil {
		self.t.Fatal(err)
	}
	opts := []any{NoBudget()}
	if immediate {
		opts = append(opts, DeliverImmediately())
	}
	accepted, err := self.wire.sender.SendWithTimeoutDetailed(frame, self.wire.receiver.ClientId(),
		func(error) {}, time.Second, opts...)
	if !accepted || err != nil {
		MessagePoolReturn(owned)
		self.t.Fatalf("control original refused: accepted=%t err=%v", accepted, err)
	}
	wire := self.wire.take(self.wire.senderOut)
	if wire.pack == nil || wire.pack.DeliverImmediately != immediate {
		self.t.Fatalf("control original lost its requested delivery: immediate=%t", immediate)
	}
	self.wire.forward(wire, self.wire.receiverIn)
	self.wire.acknowledge()
	return wire
}

// The missing DATA arrives; the exit's return stream moves again.
func (self *immediateProgressFixture) repair() {
	self.t.Helper()
	self.wire.forward(self.missing, self.wire.receiverIn)
	self.wire.acknowledge()
}

// A bare exit with the state a real receive path writes.
func immediateProgressExit(settings *MultiClientSettings) *multiClientChannel {
	return &multiClientChannel{
		log:                       NewNoopLogger(),
		args:                      &multiClientChannelArgs{},
		settings:                  settings,
		packetStats:               &clientWindowStats{log: NewNoopLogger()},
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
	}
}

// Root cause: the client counted a SYN-ACK and pure ACK delivered ahead of the
// exit's receive gap as receive progress. That lifted the exit's quarantine,
// cleared its unanswered-connect evidence, refreshed its qualification and
// hid the no-receive verdict, while its DATA still waited behind the gap.
func TestClientExitQuarantineNotLiftedByImmediateControlAheadOfGap(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		settings := DefaultMultiClientSettings()
		exit := immediateProgressExit(settings)
		var localDeliveries atomic.Int32
		exit.clientReceivePacketCallback = func(*multiClientChannel, TransferPath, protocol.ProvideMode, TransportType, *IpPath, []byte) {
			localDeliveries.Add(1)
		}
		var qualificationRefreshes atomic.Int32
		exit.qualificationRefreshFunc = func(MultiHopId) {
			qualificationRefreshes.Add(1)
		}
		fixture := newImmediateProgressFixture(t, exit)

		// A soft no-receive verdict quarantined the exit: its sends are acked,
		// nothing has come back, and a connect is unanswered.
		now := time.Now()
		func() {
			exit.stateLock.Lock()
			defer exit.stateLock.Unlock()
			exit.packetStats.firstSendNackTime = now.Add(-settings.BlackholeReceiveTimeout - time.Second)
			exit.packetStats.sendAckCount = 4
			exit.packetStats.sendAckByteCount = 1200
			exit.packetStats.firstUnansweredSendSynTime = now.Add(-time.Second)
		}()
		exit.setQuarantined(blackholeNoReceiveAck)

		fixture.sendControl(ipOosTcpPacketSequence(fixture.controlPath, tcpFlagSyn|tcpFlagAck, 7000, nil), true)
		fixture.sendControl(ipOosTcpPacketSequence(fixture.controlPath, tcpFlagAck, 7001, nil), true)
		if receiver := fixture.wire.receiveSequence(); receiver.nextSequenceNumber != fixture.base {
			t.Fatal("the controls moved the exit's receive floor across the missing DATA")
		}
		if localDeliveries.Load() != 2 {
			t.Fatalf("the controls did not reach the local stack ahead of the gap: delivered=%d", localDeliveries.Load())
		}

		judge := func() (bool, int, int, bool, bool, blackholeReason) {
			exit.stateLock.Lock()
			defer exit.stateLock.Unlock()
			reason, _ := blackholeReasonFromStats(time.Now(), exit.packetStats,
				settings.BlackholeTimeout, settings.BlackholeReceiveTimeout, settings.BlackholeConnectTimeout, blackholeGates{})
			return exit.quarantined, exit.packetStats.receiveAckCount, exit.packetStats.receiveSynCount,
				!exit.lastReceiveAckTime.IsZero(), exit.packetStats.firstUnansweredSendSynTime.IsZero(), reason
		}
		quarantined, receiveAckCount, receiveSynCount, received, answered, reason := judge()
		if !quarantined || receiveAckCount != 0 || receiveSynCount != 0 || received || answered ||
			qualificationRefreshes.Load() != 0 || reason != blackholeNoReceiveAck {
			t.Errorf("CONTROL_AHEAD_OF_GAP_COUNTED_AS_EXIT_PROGRESS: quarantined=%t receive_acks=%d receive_syns=%d receive_stamped=%t connect_answered=%t qualification_refreshes=%d verdict=%q",
				quarantined, receiveAckCount, receiveSynCount, received, answered, qualificationRefreshes.Load(), reason)
		}

		// Positive control: the DATA arriving is progress and acquits.
		fixture.repair()
		quarantined, receiveAckCount, _, received, _, reason = judge()
		if quarantined || receiveAckCount < 1 || !received || reason != blackholeNone {
			t.Errorf("DATA progress did not lift the quarantine: quarantined=%t receive_acks=%d receive_stamped=%t verdict=%q",
				quarantined, receiveAckCount, received, reason)
		}
	})
}

// Root cause, readiness: a probe's SYN-ACK delivered ahead of the exit's
// receive gap answered the probe and would qualify the exit for user flows
// while its return stream was stuck.
func TestClientProbeNotAnsweredByImmediateSynAckAheadOfGap(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		parent, _, forwarded := probeTestParent(t)
		exit := immediateProgressExit(parent.settings)
		exit.clientReceivePacketCallback = parent.clientReceivePacket
		fixture := newImmediateProgressFixture(t, exit)
		probe, ok := parent.registerProbeFlow(exit, probeHostTarget("probe-target.example", net.ParseIP("203.0.113.10")))
		if !ok {
			t.Fatal("probe flow was not registered")
		}
		answer := func() []byte {
			_, packet := probeTestSynAck(t, probe.ipPath, 0x5150)
			return packet
		}
		// Ordinary return DATA may reach the application; probe traffic never.
		probeForwarded := func() int {
			count := 0
			for _, p := range *forwarded {
				if probeIngressPath(p.IpPath) || probeEgressPath(p.IpPath) {
					count++
				}
			}
			return count
		}

		fixture.sendControl(answer(), true)
		answered := false
		select {
		case <-probe.done:
			answered = probe.answered.Load()
		default:
		}
		exit.stateLock.Lock()
		connectSuccesses := len(exit.connectSuccessTimes)
		exit.stateLock.Unlock()
		if answered || connectSuccesses != 0 {
			t.Errorf("PROBE_ANSWERED_AHEAD_OF_GAP: a probe SYN-ACK delivered ahead of the exit's receive gap answered the probe: answered=%t connect_successes=%d",
				answered, connectSuccesses)
		}
		if n := probeForwarded(); n != 0 {
			t.Fatalf("%d probe packet(s) reached the application", n)
		}

		// Positive control: once the gap repairs, an answer at the floor
		// counts, even when its sender asked for immediate delivery.
		fixture.repair()
		fixture.sendControl(answer(), true)
		select {
		case <-probe.done:
		default:
			t.Fatal("an answer at the receive floor did not complete the probe")
		}
		if !probe.answered.Load() {
			t.Error("an answer at the receive floor did not answer the probe")
		}
		if n := probeForwarded(); n != 0 {
			t.Fatalf("%d probe packet(s) reached the application", n)
		}
	})
}
