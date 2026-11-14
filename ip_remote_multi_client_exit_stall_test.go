package connect

// ip_remote_multi_client_exit_stall_test.go -- an exit that has stopped making
// progress (ip_remote_multi_client_exit_stall.go), against the root causes in
// the owner's iOS logs of 2026-11-13:
//
//   - an exit that never acks holds its flows until the send-ack lifetime
//     (MultiClientSettings.AckTimeout) closes its sequence: 15 exits, 70% of
//     the flows reset, each after up to 30 s;
//   - a receive gap freezes an exit's whole return stream until GapTimeout
//     ends the sequence, and the exit stayed in its window after that;
//   - new flows and affinity donation kept landing on an exit stalled for
//     seconds;
//   - a restart restored an exit that was stalling before the kill, which
//     then absorbed SYNs for 10 s;
//   - a provider removed while its candidate was evaluated was still
//     installed (connect f9439015 follow-up a);
//   - a saved identity with an empty destination reached a window, which
//     panics on it (follow-up b).
//
// The window tests run a real multi client over in-process providers (the
// rerace harness) in synctest time. The app values of the exit timers are the
// sdk's (app_window_exit_timers.go); here they are window settings. Every id,
// address and credential is synthetic.

import (
	"context"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The exit timers of an app window (sdk app_window_exit_timers.go).
const (
	exitStallTestAppAckTimeout = 12 * time.Second
	exitStallTestAppGapTimeout = 15 * time.Second
)

// How long after the deadline it pins an outcome may land. Nothing on these
// paths waits on a timer of its own, so in synctest time the outcome lands at
// the deadline; the margin only keeps the assertions readable.
const exitStallTestSettle = 100 * time.Millisecond

// Sizes the quality window to count exits with no standing reserve and no
// surplus evaluations, as an iOS window holds its size, so its resize pass is
// waiting, not expanding against providers that do not exist, when an exit
// leaves, and reaps it at once. configure, when set, adjusts the rest.
func exitStallWindowSettings(count int, configure func(*MultiClientSettings)) func(*MultiClientSettings) {
	return func(settings *MultiClientSettings) {
		settings.StandingReserve = false
		settings.EvaluationPoolMultiple = 1
		windowSize := settings.WindowSizes[WindowTypeQuality]
		windowSize.WindowSizeMin = count
		windowSize.WindowSizeMax = count
		windowSize.WindowSizeHardMax = count
		windowSize.WindowSizeMinIpv6Capable = 0
		settings.WindowSizes[WindowTypeQuality] = windowSize
		if configure != nil {
			configure(settings)
		}
	}
}

// The quality window's live channel to the named exit, nil when it holds none.
func exitStallChannel(harness *reraceHarness, name string) *multiClientChannel {
	exit := harness.exit(name)
	for _, client := range harness.multiClient.windows[WindowTypeQuality].unorderedClients() {
		if client.Destination() == exit.destination && !client.IsDone() {
			return client
		}
	}
	return nil
}

// Whether the app received a tcp reset for the flow: the teardown signal a
// removed exit's tcp flows get, so the app reconnects through another exit.
func exitStallResetDelivered(harness *reraceHarness, path *IpPath) bool {
	for _, packet := range harness.delivered(path) {
		if parsed, err := ParseIpPath(packet.packet); err == nil && parsed.Protocol == IpProtocolTcp && parsed.Rst {
			return true
		}
	}
	return false
}

// A fresh app tcp flow to destination port, so the flow starts an affinity
// group of its own (a port under 1024 groups by port alone).
func exitStallFlow(destinationIndex int, sourcePort int, destinationPort int) *IpPath {
	path := reraceFlow(4, destinationIndex, sourcePort)
	path.DestinationPort = destinationPort
	return path
}

// The app's keepalive datagram on a flow the warm exit carries.
func exitStallKeepalivePath() *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.IPv4(10, 1, 0, 2).To4(),
		SourcePort:      49000,
		DestinationIp:   net.IPv4(198, 51, 100, 9).To4(),
		DestinationPort: 123,
	}
}

// The identity of one transmitted Pack: retransmissions repeat it.
type exitStallPackKey struct {
	sequenceId     Id
	logicalLane    uint32
	sequenceNumber uint64
}

// The key of an acknowledged Pack that carries a provider's return packets,
// false for anything else: acknowledgements, controls, and no-ack datagrams,
// which a receive sequence never holds behind a hole.
func exitStallDataPackKey(frameBytes []byte) (exitStallPackKey, bool) {
	var transfer protocol.TransferFrame
	if ProtoUnmarshal(frameBytes, &transfer) != nil {
		return exitStallPackKey{}, false
	}
	pack := transfer.Pack
	if pack == nil && transfer.Frame != nil && transfer.Frame.MessageType == protocol.MessageType_TransferPack {
		pack = &protocol.Pack{}
		if ProtoUnmarshal(transfer.Frame.MessageBytes, pack) != nil {
			return exitStallPackKey{}, false
		}
	}
	if pack == nil || pack.Nack {
		return exitStallPackKey{}, false
	}
	data := false
	for _, frame := range pack.Frames {
		if frame != nil && frame.MessageType == protocol.MessageType_IpIpPacketFromProvider {
			data = true
		}
	}
	if !data {
		return exitStallPackKey{}, false
	}
	sequenceId, err := IdFromBytes(pack.SequenceId)
	if err != nil {
		return exitStallPackKey{}, false
	}
	return exitStallPackKey{
		sequenceId:     sequenceId,
		logicalLane:    pack.LogicalLane,
		sequenceNumber: pack.SequenceNumber,
	}, true
}

// The hole of the owner's logs: a provider whose return stream loses one item
// for good. While armed, the first acknowledged return Pack the provider sends
// is dropped, and so is every retransmission of it, while every other frame,
// acknowledgements included, passes in order. Disarmed, the next
// retransmission passes and fills the hole. Safe for concurrent use; one relay
// goroutine runs per source client.
type exitStallGapRelay struct {
	stateLock sync.Mutex
	armed     bool
	target    exitStallPackKey
	targetSet bool
	// closed when a transmission of the target passes after disarm
	filled     chan struct{}
	filledOnce sync.Once
}

func newExitStallGapRelay() *exitStallGapRelay {
	return &exitStallGapRelay{filled: make(chan struct{})}
}

// Drops the next return Pack and its retransmissions.
func (self *exitStallGapRelay) arm() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.armed = true
}

// Lets the dropped Pack's next retransmission through.
func (self *exitStallGapRelay) disarm() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.armed = false
}

// Whether a Pack was chosen to drop.
func (self *exitStallGapRelay) holeOpen() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.targetSet
}

func (self *exitStallGapRelay) drop(frameBytes []byte) bool {
	key, ok := exitStallDataPackKey(frameBytes)
	if !ok {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.targetSet {
		if !self.armed {
			return false
		}
		self.target, self.targetSet = key, true
		return true
	}
	if key != self.target {
		return false
	}
	if self.armed {
		return true
	}
	self.filledOnce.Do(func() {
		close(self.filled)
	})
	return false
}

func (self *exitStallGapRelay) relay(ctx context.Context, provider <-chan []byte, source chan<- []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-provider:
			if self.drop(frame) {
				MessagePoolReturn(frame)
				continue
			}
			select {
			case source <- frame:
			case <-ctx.Done():
				MessagePoolReturn(frame)
				return
			}
		}
	}
}

// A slow provider: every frame to its source client, acknowledgements
// included, arrives delay late and in order.
type exitStallDelayRelay struct {
	delay time.Duration
}

func (self *exitStallDelayRelay) relay(ctx context.Context, provider <-chan []byte, source chan<- []byte) {
	type delayedFrame struct {
		at    time.Time
		frame []byte
	}
	var pending []delayedFrame
	defer func() {
		for _, delayed := range pending {
			MessagePoolReturn(delayed.frame)
		}
	}()
	for {
		var out chan<- []byte
		var next []byte
		var due <-chan time.Time
		if 0 < len(pending) {
			if wait := time.Until(pending[0].at); wait <= 0 {
				out, next = source, pending[0].frame
			} else {
				due = time.After(wait)
			}
		}
		select {
		case <-ctx.Done():
			return
		case frame := <-provider:
			pending = append(pending, delayedFrame{at: time.Now().Add(self.delay), frame: frame})
		case out <- next:
			pending = pending[1:]
		case <-due:
		}
	}
}

// The test providers with the API generator's exclusions
// (MultiClientGeneratorExcluder, multiClientGeneratorExclusions): an excluded
// provider is listed no more, and each client the window builds is announced
// on built with its provider's client id.
type exitStallExclusionGenerator struct {
	*reraceGenerator
	built chan Id

	stateLock sync.Mutex
	excluded  map[Id]bool
}

func newExitStallExclusionGenerator(inner *reraceGenerator) *exitStallExclusionGenerator {
	return &exitStallExclusionGenerator{
		reraceGenerator: inner,
		built:           make(chan Id, 64),
		excluded:        map[Id]bool{},
	}
}

func (self *exitStallExclusionGenerator) ExcludeClientId(clientId Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.excluded[clientId] = true
}

func (self *exitStallExclusionGenerator) excludesDestination(destination MultiHopId) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, clientId := range destination.Ids() {
		if self.excluded[clientId] {
			return true
		}
	}
	return false
}

func (self *exitStallExclusionGenerator) filter(destinations map[MultiHopId]DestinationStats) map[MultiHopId]DestinationStats {
	for destination := range destinations {
		if self.excludesDestination(destination) {
			delete(destinations, destination)
		}
	}
	return destinations
}

func (self *exitStallExclusionGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	destinations, err := self.reraceGenerator.NextDestinations(count, excludeDestinations, rankMode)
	return self.filter(destinations), err
}

func (self *exitStallExclusionGenerator) NextDestinationsWithIpFamily(count int, excludeDestinations []MultiHopId, rankMode string, ipFamily IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	destinations, err := self.reraceGenerator.NextDestinationsWithIpFamily(count, excludeDestinations, rankMode, ipFamily)
	return self.filter(destinations), err
}

func (self *exitStallExclusionGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	self.reraceGenerator.mutex.Lock()
	exit := self.reraceGenerator.clientExits[args.ClientId]
	self.reraceGenerator.mutex.Unlock()
	client, err := self.reraceGenerator.NewClient(ctx, args, clientSettings)
	if err == nil && exit != nil {
		select {
		case self.built <- exit.provider.ClientId():
		default:
		}
	}
	return client, err
}

// Root cause 1, the mechanism. An exit that takes packets and acknowledges
// none of them holds every flow on it until the window's send-ack lifetime
// closes its sequence: 30 s in the owner's logs, where the busy probe held the
// stall verdict each time ("liveness probe unanswered"). The app windows now
// run a 12 s lifetime (the sdk's TestAppWindowsRunTheShortExitTimers pins the
// value and fails on the old one); this pins what the value buys. The exit
// stays until the lifetime, is gone at it, and its flows re-home: the
// established quic flow moves to the healthy exit, the tcp flow gets its
// reset, and the app's reconnect lands on the healthy exit at once.
func TestNeverAckingExitIsEvictedAtTheWindowAckTimeoutAndItsFlowsRehome(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		hold := newReraceProviderHold()
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "stall", tier: 0, providerHold: hold},
			{name: "healthy", tier: 3},
		}, exitStallWindowSettings(2, func(settings *MultiClientSettings) {
			settings.AckTimeout = exitStallTestAppAckTimeout
		}))
		t.Cleanup(func() { hold.set(false) })
		stall := harness.channel("stall")
		healthy := harness.channel("healthy")

		tcp := exitStallFlow(1, 41001, 443)
		if _, _, ok := harness.connect(tcp); !ok {
			t.Fatal("the tcp flow never connected")
		}
		quic := reraceUdpFlow(4, 2, 41002)
		harness.send(ipOosUdpPacket(quic, []byte("quic initial")))
		if len(harness.delivered(quic)) == 0 {
			t.Fatal("the quic flow was never answered")
		}
		if harness.boundTo(tcp) != stall || harness.boundTo(quic) != stall {
			t.Fatal("the flows did not start on the exit that stalls")
		}

		// the exit takes every packet from here and acknowledges none
		hold.set(true)
		start := time.Now()

		offset := uint32(0)
		for time.Since(start) < exitStallTestAppAckTimeout-500*time.Millisecond {
			harness.send(reraceSegment(tcp, offset, []byte("segment")))
			offset += uint32(len("segment"))
			harness.send(ipOosUdpPacket(quic, []byte("quic data")))
			harness.sleep(500 * time.Millisecond)
		}
		if exitStallChannel(harness, "stall") != stall {
			t.Fatalf("the exit left its window %s after it stopped acking, before the %s lifetime", time.Since(start), exitStallTestAppAckTimeout)
		}
		harness.sleep(time.Until(start.Add(exitStallTestAppAckTimeout + exitStallTestSettle)))
		if exitStallChannel(harness, "stall") != nil {
			t.Fatalf("the never-acking exit was still in its window %s after it stopped acking", time.Since(start))
		}

		// re-homed: the established quic flow moved, the tcp flow was reset
		if got := harness.boundTo(quic); got != healthy {
			t.Errorf("the quic flow is bound to %v after the eviction, want the healthy exit", got)
		}
		if harness.boundTo(tcp) != nil {
			t.Error("the tcp flow is still bound after its exit left")
		}
		if !exitStallResetDelivered(harness, tcp) {
			t.Error("the app got no reset for the tcp flow of the evicted exit")
		}
		reconnect := exitStallFlow(1, 41003, 443)
		synCount, _, ok := harness.connect(reconnect)
		if !ok || synCount != 1 || harness.boundTo(reconnect) != healthy {
			t.Errorf("the reconnect took %d SYNs (ok %t) to %v, want one SYN to the healthy exit", synCount, ok, harness.boundTo(reconnect))
		}
	})
}

// Root cause 2. A hole in an exit's return stream freezes every flow on the
// exit: in the owner's logs four exits held one for the whole 60 s
// GapTimeout, one of them carrying 65 flows, and stayed in their window after
// the timeout ended the sequence, because nothing told the window. The app
// windows run a 15 s GapTimeout (the sdk pins it), and the timeout is now an
// eviction notice: the exit leaves its window at the timeout, while the
// tunnel receives from another exit, and its flows re-home.
func TestGapWedgedExitIsEvictedAtTheGapTimeoutAndItsFlowsRehome(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "warm", tier: 3},
		}, exitStallWindowSettings(2, nil))
		warm := harness.channel("warm")
		// the tunnel keeps receiving through the warm exit
		keepalive := exitStallKeepalivePath()
		harness.send(ipOosUdpPacket(keepalive, []byte("keepalive")))
		if harness.boundTo(keepalive) != warm {
			t.Fatal("the keepalive flow is not on the warm exit")
		}

		gap := newExitStallGapRelay()
		harness.addExit(reraceExitSpec{
			name:          "gapped",
			tier:          0,
			providerRelay: gap,
			clientSettings: func(settings *ClientSettings) {
				settings.ReceiveBufferSettings.GapTimeout = exitStallTestAppGapTimeout
			},
		})
		harness.waitForExits(2)
		gapped := harness.channel("gapped")

		tcp := exitStallFlow(1, 42001, 443)
		if _, _, ok := harness.connect(tcp); !ok {
			t.Fatal("the tcp flow never connected")
		}
		quic := reraceUdpFlow(4, 2, 42002)
		harness.send(ipOosUdpPacket(quic, []byte("quic initial")))
		if harness.boundTo(tcp) != gapped || harness.boundTo(quic) != gapped || len(harness.delivered(quic)) == 0 {
			t.Fatal("the flows did not start on the exit that holds the hole")
		}

		// the first return item after this is lost for good, the next passes
		gap.arm()
		harness.send(reraceSegment(tcp, 0, []byte("first")))
		if !gap.holeOpen() {
			t.Fatal("the provider's first return item was not dropped")
		}
		harness.send(reraceSegment(tcp, uint32(len("first")), []byte("second")))
		holeStart := time.Now()

		for time.Since(holeStart) < exitStallTestAppGapTimeout-500*time.Millisecond {
			harness.send(ipOosUdpPacket(keepalive, []byte("keepalive")))
			harness.sleep(500 * time.Millisecond)
		}
		if exitStallChannel(harness, "gapped") != gapped {
			t.Fatalf("the exit left its window %s after the hole opened, before the %s gap timeout", time.Since(holeStart), exitStallTestAppGapTimeout)
		}
		harness.send(ipOosUdpPacket(keepalive, []byte("keepalive")))
		harness.sleep(time.Until(holeStart.Add(exitStallTestAppGapTimeout + exitStallTestSettle)))
		if exitStallChannel(harness, "gapped") != nil {
			t.Fatalf("the gap-wedged exit was still in its window %s after the hole opened, past the %s gap timeout", time.Since(holeStart), exitStallTestAppGapTimeout)
		}

		if got := harness.boundTo(quic); got != warm {
			t.Errorf("the quic flow is bound to %v after the eviction, want the warm exit", got)
		}
		if harness.boundTo(tcp) != nil {
			t.Error("the tcp flow is still bound after its exit left")
		}
		if !exitStallResetDelivered(harness, tcp) {
			t.Error("the app got no reset for the tcp flow of the evicted exit")
		}
	})
}

// Root cause 3, an ack stall. While an exit acknowledged nothing, new flows
// and affinity donation kept landing on it, each to wait out the stall. Once
// its outstanding sends have made no ack progress for SendStallTimeout (3 s)
// and the tunnel receives, it takes no new flow: neither a flow of the
// affinity group it carries (donation) nor a flow of a new group (the
// window's offer), though it is the best rank. Its first acks put it back.
func TestAckStalledExitTakesNoNewFlowsUntilItAcksAgain(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		hold := newReraceProviderHold()
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "stall", tier: 0, providerHold: hold},
			{name: "healthy", tier: 3},
		}, exitStallWindowSettings(2, nil))
		t.Cleanup(func() { hold.set(false) })
		stall := harness.channel("stall")
		healthy := harness.channel("healthy")

		tcp := exitStallFlow(1, 43001, 443)
		if _, _, ok := harness.connect(tcp); !ok || harness.boundTo(tcp) != stall {
			t.Fatal("the tcp flow did not connect through the best-ranked exit")
		}

		hold.set(true)
		harness.send(reraceSegment(tcp, 0, []byte("unacked")))
		harness.sleep(3500 * time.Millisecond)

		// a flow of the group the stalled exit carries, and one of a new group
		donated := exitStallFlow(1, 43002, 443)
		offered := exitStallFlow(3, 43003, 993)
		for _, path := range []*IpPath{donated, offered} {
			harness.syn(path)
			if got := harness.boundTo(path); got != healthy {
				t.Errorf("a new flow to port %d went to %v 3.5 s into the stall, want the healthy exit", path.DestinationPort, got)
			}
			if got := len(harness.exit("stall").syns(path)); got != 0 {
				t.Errorf("the stalled exit took %d SYNs of a new flow to port %d", got, path.DestinationPort)
			}
		}

		// progress resumes: the held acks arrive
		hold.set(false)
		harness.settle()
		back := exitStallFlow(5, 43005, 995)
		if _, _, ok := harness.connect(back); !ok || harness.boundTo(back) != stall {
			t.Errorf("a new flow after the acks resumed went to %v, want the best-ranked exit again", harness.boundTo(back))
		}
	})
}

// Root cause 3, a receive gap. While a hole froze an exit's return stream,
// new flows and affinity donation kept landing on it. Once the hole has been
// open for ReceiveGapStallTimeout (3 s) and the tunnel receives, the exit takes
// no new flow, of its own affinity group or of a new one. The filled hole puts
// it back.
func TestGapStalledExitTakesNoNewFlowsUntilTheHoleFills(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "warm", tier: 3},
		}, exitStallWindowSettings(2, nil))
		warm := harness.channel("warm")
		keepalive := exitStallKeepalivePath()
		harness.send(ipOosUdpPacket(keepalive, []byte("keepalive")))

		gap := newExitStallGapRelay()
		harness.addExit(reraceExitSpec{name: "gapped", tier: 0, providerRelay: gap})
		harness.waitForExits(2)
		gapped := harness.channel("gapped")

		tcp := exitStallFlow(1, 44001, 443)
		if _, _, ok := harness.connect(tcp); !ok || harness.boundTo(tcp) != gapped {
			t.Fatal("the tcp flow did not connect through the best-ranked exit")
		}
		gap.arm()
		harness.send(reraceSegment(tcp, 0, []byte("first")))
		harness.send(reraceSegment(tcp, uint32(len("first")), []byte("second")))
		if !gap.holeOpen() {
			t.Fatal("the provider's first return item was not dropped")
		}
		for range 7 {
			harness.send(ipOosUdpPacket(keepalive, []byte("keepalive")))
			harness.sleep(500 * time.Millisecond)
		}

		donated := exitStallFlow(1, 44002, 443)
		offered := exitStallFlow(3, 44003, 993)
		for _, path := range []*IpPath{donated, offered} {
			harness.syn(path)
			if got := harness.boundTo(path); got != warm {
				t.Errorf("a new flow to port %d went to %v 3.5 s into the hole, want the warm exit", path.DestinationPort, got)
			}
			if got := len(harness.exit("gapped").syns(path)); got != 0 {
				t.Errorf("the gap-stalled exit took %d SYNs of a new flow to port %d", got, path.DestinationPort)
			}
		}

		// the lost item's next retransmission passes and fills the hole
		gap.disarm()
		select {
		case <-gap.filled:
		case <-time.After(30 * time.Second):
			t.Fatal("the provider never retransmitted the lost item")
		}
		harness.settle()
		back := exitStallFlow(5, 44005, 995)
		if _, _, ok := harness.connect(back); !ok || harness.boundTo(back) != gapped {
			t.Errorf("a new flow after the hole filled went to %v, want the best-ranked exit again", harness.boundTo(back))
		}
	})
}

// The guard against churn: these rules act only on an exit that has stopped,
// never on one that is merely slow. A provider whose every frame arrives
// 1.5 s late, under the app windows' timers (12 s send-ack lifetime, 15 s
// gap timeout), carrying a steady upload for a minute, keeps acking and
// delivering in order: it is never evicted, and it keeps taking new flows as
// the best rank. Passes before and after the change.
func TestSlowButProgressingExitIsNeverEvictedNorHeld(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{
				name:          "slow",
				tier:          0,
				providerRelay: &exitStallDelayRelay{delay: 1500 * time.Millisecond},
				clientSettings: func(settings *ClientSettings) {
					settings.ReceiveBufferSettings.GapTimeout = exitStallTestAppGapTimeout
				},
			},
			{name: "fast", tier: 3},
		}, exitStallWindowSettings(2, func(settings *MultiClientSettings) {
			settings.AckTimeout = exitStallTestAppAckTimeout
		}))
		slow := harness.channel("slow")

		tcp := exitStallFlow(1, 45001, 443)
		if _, _, ok := harness.connect(tcp); !ok || harness.boundTo(tcp) != slow {
			t.Fatal("the tcp flow did not connect through the slow exit")
		}
		start := time.Now()
		offset := uint32(0)
		nextFlow := start.Add(10 * time.Second)
		port := 100
		for time.Since(start) < time.Minute {
			harness.send(reraceSegment(tcp, offset, []byte("upload")))
			offset += uint32(len("upload"))
			if !time.Now().Before(nextFlow) {
				path := exitStallFlow(4, 45000+port, port)
				harness.syn(path)
				if got := len(harness.exit("fast").syns(path)); got != 0 {
					t.Errorf("a new flow %s into the run went to the fast exit: the slow exit was held", time.Since(start))
				}
				if got := len(harness.exit("slow").syns(path)); got == 0 {
					t.Errorf("a new flow %s into the run never reached the slow exit", time.Since(start))
				}
				nextFlow = nextFlow.Add(10 * time.Second)
				port += 1
			}
			harness.sleep(250 * time.Millisecond)
			if exitStallChannel(harness, "slow") != slow {
				t.Fatalf("the slow exit left its window %s into the run", time.Since(start))
			}
		}
		if harness.boundTo(tcp) != slow {
			t.Error("the upload left the slow exit")
		}
	})
}

// A window client of the test window over a minted identity.
func exitStallTestChannel(ctx context.Context, settings *MultiClientSettings, destination MultiHopId, args *MultiClientGeneratorClientArgs) *multiClientChannel {
	client := &multiClientChannel{
		log:      NewNoopLogger(),
		settings: settings,
		args: &multiClientChannelArgs{
			MultiClientGeneratorClientArgs: *args,
			Destination:                    destination,
		},
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{log: NewNoopLogger()},
	}
	client.ctx, client.cancel = context.WithCancel(ctx)
	return client
}

// A synthetic app packet to a documentation address.
func exitStallTestPath() *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.IPv4(10, 1, 0, 2).To4(),
		SourcePort:      46001,
		DestinationIp:   net.IPv4(203, 0, 113, 40).To4(),
		DestinationPort: 443,
	}
}

// The destinations of a store snapshot.
func exitStallPersistedDestinations(identities []*WindowClientIdentity) map[MultiHopId]bool {
	destinations := map[MultiHopId]bool{}
	for _, identity := range identities {
		destinations[identity.Destination] = true
	}
	return destinations
}

// Root cause 4. After the jetsam kill of 21:07:28Z the restart restored an
// exit that had been gapped and held (its liveness probe unanswered) before
// the kill, and it absorbed SYNs for 10 s. The window keeps the identity of an
// exit stalled in a warm window out of the snapshot a restart restores, and
// puts it back once the exit acks again. Driven through the window's own
// stall watchdog and the API generator's store, with the restart as a second
// generator over the snapshot the first one left.
func TestStalledExitIsNotRestoredAfterRestart(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		store := &fakeIdentityStore{}
		fixture := newModernConfirmFixture(t, ctx, []*ProviderSpec{{BestAvailable: true}}, nil, testingExclusionPlatform(nil))
		fixture.generator.SetIdentityStore(store)
		stalledDestination := RequireMultiHopId(NewId())
		healthyDestination := RequireMultiHopId(NewId())
		stalledArgs, err := fixture.generator.NewClientArgsForDestinationContext(ctx, stalledDestination)
		if err != nil {
			t.Fatal(err)
		}
		healthyArgs, err := fixture.generator.NewClientArgsForDestinationContext(ctx, healthyDestination)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		AssertEqual(t, exitStallPersistedDestinations(store.snapshot()), map[MultiHopId]bool{stalledDestination: true, healthyDestination: true})

		settings := DefaultMultiClientSettings()
		stalled := exitStallTestChannel(ctx, settings, stalledDestination, stalledArgs)
		healthy := exitStallTestChannel(ctx, settings, healthyDestination, healthyArgs)
		// held, as in the owner's logs: the exit's liveness probe is admitted
		// and never answered, so the watchdog keeps it in the window
		stalled.busyProbeSendFunc = func(time.Duration, func(error)) (bool, error) {
			return true, nil
		}
		var ingressLock sync.Mutex
		lastIngress := time.Time{}
		window := familyTestWindow(ctx, fixture.generator, settings)
		window.clients[stalledArgs.ClientId] = stalled
		window.clients[healthyArgs.ClientId] = healthy
		window.uplinkLastIngressFunc = func() time.Time {
			ingressLock.Lock()
			defer ingressLock.Unlock()
			return lastIngress
		}
		go window.watchSendStalls()

		// the healthy exit keeps receiving; the stalled one takes a packet and
		// never acks it
		stalled.addSend(1440, exitStallTestPath())
		for range 10 {
			healthy.addReceiveAck(1440)
			func() {
				ingressLock.Lock()
				defer ingressLock.Unlock()
				lastIngress = time.Now()
			}()
			time.Sleep(500 * time.Millisecond)
			synctest.Wait()
		}
		if stalled.IsDone() {
			t.Fatal("the watchdog convicted the held exit; the owner's exit stayed in its window")
		}

		// the kill: a new process restores what the store holds
		restarted := newModernConfirmFixture(t, ctx, []*ProviderSpec{{BestAvailable: true}}, store.snapshot(), testingExclusionPlatform(nil))
		if got := testingRestoredDestinationSet(restarted.generator); got[stalledDestination] || !got[healthyDestination] {
			t.Errorf("a restart restores %v, want only the healthy exit: the stalled exit was restored", got)
		}

		// the exit acks again, and a restart would restore it
		stalled.addSendAck(1440)
		for range 4 {
			healthy.addReceiveAck(1440)
			func() {
				ingressLock.Lock()
				defer ingressLock.Unlock()
				lastIngress = time.Now()
			}()
			time.Sleep(500 * time.Millisecond)
			synctest.Wait()
		}
		AssertEqual(t, exitStallPersistedDestinations(store.snapshot()), map[MultiHopId]bool{stalledDestination: true, healthyDestination: true})
		stalled.Cancel()
		healthy.Cancel()
	})
}

// Follow-up a of connect f9439015. RemoveProvider excludes the provider and
// cancels the window's clients of it, and the expansion refuses a candidate
// listed before the removal, but a candidate already being evaluated when the
// provider is removed is in no window yet, so the cut misses it, and it was
// installed once its evaluation succeeded. The install now reads the
// exclusion under the lock the cut takes.
func TestProviderRemovedWhileItsCandidateIsEvaluatedIsNeverInstalled(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		var generator *exitStallExclusionGenerator
		harness := newReraceHarnessWithGenerator(t, []reraceExitSpec{
			{name: "kept", tier: 1},
		}, exitStallWindowSettings(2, nil), func(inner *reraceGenerator) MultiClientGenerator {
			generator = newExitStallExclusionGenerator(inner)
			return generator
		})
		hold := newReraceProviderHold()
		hold.set(true)
		t.Cleanup(func() { hold.set(false) })
		removed := harness.addExit(reraceExitSpec{name: "removed", tier: 0, providerHold: hold})

		// the window builds a candidate for the provider, whose evaluation
		// ping waits on the held provider
		removedId := removed.provider.ClientId()
		watchdog := time.After(2 * time.Minute)
		for building := true; building; {
			select {
			case clientId := <-generator.built:
				building = clientId != removedId
			case <-watchdog:
				t.Fatal("the window never built a candidate for the provider")
			}
		}
		synctest.Wait()
		AssertEqual(t, exitStallChannel(harness, "removed"), (*multiClientChannel)(nil))

		// the user removes the provider mid-evaluation, then its answer lands
		harness.multiClient.RemoveProvider(removedId)
		hold.set(false)
		harness.sleep(time.Second)
		if exitStallChannel(harness, "removed") != nil {
			t.Fatal("the removed provider was installed after its evaluation")
		}
		harness.sleep(time.Minute)
		if exitStallChannel(harness, "removed") != nil {
			t.Fatal("the removed provider was installed later")
		}
		if exitStallChannel(harness, "kept") == nil {
			t.Fatal("the kept provider left its window")
		}
	})
}

// Follow-up b of connect f9439015. A corrupt saved identity that names no
// destination was restored, listed with post-quantum off, and a window that
// listed it panicked on the empty destination (MultiHopId.Tail). It is dropped
// at load and forgotten, with post-quantum off and on, and every listing path
// and every window lists the other restored destination as before.
func TestRestoredIdentityWithAnEmptyDestinationIsDroppedWithoutPanic(t *testing.T) {
	testRestoredIdentityWithAnEmptyDestinationIsDroppedWithoutPanic(t, false)
}

func TestRestoredIdentityWithAnEmptyDestinationIsDroppedWithoutPanicUnderPostQuantum(t *testing.T) {
	testRestoredIdentityWithAnEmptyDestinationIsDroppedWithoutPanic(t, true)
}

func testRestoredIdentityWithAnEmptyDestinationIsDroppedWithoutPanic(t *testing.T, postQuantum bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	kept := RequireMultiHopId(NewId())
	corrupt := &WindowClientIdentity{
		ClientId:    NewId(),
		ByJwt:       "synthetic-corrupt",
		InstanceId:  NewId(),
		Destination: MultiHopId{},
	}
	fixture := newModernConfirmFixture(
		t,
		ctx,
		[]*ProviderSpec{{BestAvailable: true}},
		[]*WindowClientIdentity{corrupt, testingRestoredIdentity(kept, "synthetic-kept")},
		testingExclusionPlatform(nil),
	)
	fixture.generator.SetPostQuantumEncryption(postQuantum)
	if got := testingRestoredDestinationSet(fixture.generator); got[MultiHopId{}] || !got[kept] {
		t.Errorf("the identities pending reuse are for %v, want only the kept destination", got)
	}

	for _, call := range testingEveryDiscoveryCall(ctx, fixture.generator, nil) {
		destinations, err := call()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := destinations[MultiHopId{}]; ok {
			t.Error("a listing returned the empty destination")
		}
	}

	window := capabilityTestWindow(ctx, fixture.generator, DefaultMultiClientSettings(), WindowTypeQuality)
	window.setRequestSizing(2, 0)
	var enumerated []enumeratedDestination
	var enumerateErr error
	var panicked any
	func() {
		defer func() {
			panicked = recover()
		}()
		enumerated, enumerateErr = window.enumerateDestinations(nil, false)
	}()
	if panicked != nil {
		t.Fatalf("the window panicked listing the restored destinations: %v", panicked)
	}
	AssertEqual(t, enumerateErr, nil)
	listedKept := false
	for _, destination := range enumerated {
		if destination.destination.Len() == 0 {
			t.Error("the window listed the empty destination")
		}
		listedKept = listedKept || destination.destination == kept
	}
	if !listedKept {
		t.Error("the window did not list the kept restored destination")
	}
	// forgotten: no later dial of the empty destination takes it
	if identity := fixture.generator.identityState.TakeRestored(MultiHopId{}); identity != nil {
		t.Error("the corrupt identity is still pending reuse")
	}
}
