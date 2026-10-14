package connect

// ip_remote_multi_client_rerace_test.go -- the multi client's re-race on a
// provider dial-failure signal (clientDialFailure), end to end: a real
// RemoteUserNatMultiClient with its windows and channels over in-process
// provider Clients. Each provider answers a SYN with a SYN-ACK, with the
// signal a provider sends when its upstream dial fails (ipOosUnreachable over
// its flow path, sequence zero; see TestRemoteUserNatProviderDialFailureSignalReachesClient),
// or with nothing. Every packet stays in the process and time is synctest's.
// The IDs RR-1..RR-12 are the items of the re-race test matrix.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// reraceAnswer is how a test provider answers a connection attempt.
type reraceAnswer int

const (
	// a SYN-ACK, then an echo of every data segment
	reraceAccept reraceAnswer = iota
	// the provider's dial-failure signal
	reraceSignal
	// nothing, like a provider that predates the signal
	reraceSilent
)

func reraceAlways(answer reraceAnswer) func(*IpPath) reraceAnswer {
	return func(*IpPath) reraceAnswer {
		return answer
	}
}

// reraceServerIsn is the initial sequence number every test destination
// answers with.
const reraceServerIsn = uint32(7000)

// reraceSynSchedule is a Linux source's SYN schedule: the initial RTO of 1s,
// doubling per retransmit, six retransmits (tcp_syn_retries), and the connect
// timeout one more doubling after the last.
var reraceSynSchedule = []time.Duration{
	0,
	1 * time.Second,
	3 * time.Second,
	7 * time.Second,
	15 * time.Second,
	31 * time.Second,
	63 * time.Second,
}

const reraceSynTimeout = 127 * time.Second

// reraceSettle bounds how long after the packet that causes it an answer may
// land. Nothing on these paths waits on a timer, so in synctest time the
// answer lands at once; the bound only keeps the assertions readable.
const reraceSettle = 100 * time.Millisecond

type reraceExitSpec struct {
	name   string
	family IpFamily
	tier   int
	// answer decides each SYN (and each udp datagram); nil accepts all
	answer func(path *IpPath) reraceAnswer
}

// reraceExitPacket is one packet a test provider received.
type reraceExitPacket struct {
	at      time.Time
	source  Id
	path    *IpPath
	payload []byte
}

// reraceExit is one in-process provider: a Client that records every packet
// sent to it and answers per its spec. Replies leave on their own goroutine,
// since a receive callback must not block.
type reraceExit struct {
	reraceExitSpec
	t           *testing.T
	destination MultiHopId
	provider    *Client
	replies     chan reraceReply

	mutex   sync.Mutex
	packets []reraceExitPacket
}

type reraceReply struct {
	destination Id
	packet      []byte
}

func (self *reraceExit) receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	for _, frame := range frames {
		if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
			continue
		}
		packet, err := ipPacketToProviderBytes(frame)
		if err != nil {
			continue
		}
		path, payload, err := ParseIpPathWithPayload(packet)
		if err != nil {
			self.t.Errorf("%s: unparseable packet from the multi client: %s", self.name, err)
			continue
		}
		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()
			self.packets = append(self.packets, reraceExitPacket{
				at:      time.Now(),
				source:  source.SourceId,
				path:    path,
				payload: slices.Clone(payload),
			})
		}()
		if probeEgressPath(path) {
			// qualification probes go unanswered, which records nothing
			continue
		}
		if reply, ok := self.reply(path, payload); ok {
			self.sendTo(source.SourceId, reply)
		}
	}
}

func (self *reraceExit) sendTo(destination Id, packet []byte) {
	select {
	case self.replies <- reraceReply{destination: destination, packet: packet}:
	default:
		self.t.Errorf("%s: reply queue full", self.name)
	}
}

func (self *reraceExit) answerFor(path *IpPath) reraceAnswer {
	if self.answer == nil {
		return reraceAccept
	}
	return self.answer(path)
}

func (self *reraceExit) reply(path *IpPath, payload []byte) ([]byte, bool) {
	switch path.Protocol {
	case IpProtocolTcp:
		if path.Rst || path.Fin {
			return nil, false
		}
		if path.Syn && !path.Ack {
			switch self.answerFor(path) {
			case reraceAccept:
				return reraceSynAck(path), true
			case reraceSignal:
				return reraceProviderSignal(self.t, path), true
			default:
				return nil, false
			}
		}
		if 0 < len(payload) {
			return reraceTcpPacket(
				path.Reverse(),
				tcpFlagAck|tcpFlagPsh,
				reraceServerIsn+1,
				path.SequenceNumber+uint32(len(payload)),
				payload,
			), true
		}
		return nil, false
	case IpProtocolUdp:
		switch self.answerFor(path) {
		case reraceAccept:
			return ipOosUdpPacket(path.Reverse(), payload), true
		case reraceSignal:
			return reraceProviderSignal(self.t, path), true
		default:
			return nil, false
		}
	default:
		return nil, false
	}
}

func (self *reraceExit) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case reply := <-self.replies:
			frame, err := ipPacketFromProviderFrame(reply.packet, DefaultProtocolVersion)
			if err != nil {
				self.t.Errorf("%s: frame: %s", self.name, err)
				continue
			}
			self.provider.SendWithTimeout(frame, reply.destination, func(error) {}, 5*time.Second)
		}
	}
}

// received is every packet this provider was sent for the flow's 5-tuple.
func (self *reraceExit) received(path *IpPath) []reraceExitPacket {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	packets := []reraceExitPacket{}
	for _, packet := range self.packets {
		if reraceSameFlow(packet.path, path) {
			packets = append(packets, packet)
		}
	}
	return packets
}

// syns is every SYN (no ACK) for the flow this provider received.
func (self *reraceExit) syns(path *IpPath) []reraceExitPacket {
	syns := []reraceExitPacket{}
	for _, packet := range self.received(path) {
		if packet.path.Protocol == IpProtocolTcp && packet.path.Syn && !packet.path.Ack {
			syns = append(syns, packet)
		}
	}
	return syns
}

// reraceGenerator hands the quality window every test provider, with its
// category and tier, and wires each channel's client to the provider it was
// minted for (NewClientArgsForDestination) over in-memory routes. Like the
// platform it never offers a provider the client already holds, so the window
// carries one channel per provider. The speed window is offered nothing: the
// test flows are https, which the quality window carries.
type reraceGenerator struct {
	testingEmptyMultiClientGenerator

	mutex       sync.Mutex
	exits       []*reraceExit
	clientExits map[Id]*reraceExit
	unsubs      map[Id]func()
	clients     []*Client
}

func (self *reraceGenerator) exitList() []*reraceExit {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return slices.Clone(self.exits)
}

func (self *reraceGenerator) draw(families []IpFamily, count int, excludeDestinations []MultiHopId, rankMode string, stamp bool) map[MultiHopId]DestinationStats {
	destinations := map[MultiHopId]DestinationStats{}
	if rankMode != WindowTypeQuality.RankMode() {
		return destinations
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	held := map[*reraceExit]bool{}
	for _, exit := range self.clientExits {
		held[exit] = true
	}
	for _, exit := range self.exits {
		if count <= len(destinations) {
			break
		}
		if held[exit] || slices.Contains(excludeDestinations, exit.destination) {
			continue
		}
		if families != nil && !slices.Contains(families, exit.family) {
			continue
		}
		stats := DestinationStats{Tier: exit.tier}
		if stamp {
			stats.IpFamily = exit.family
		}
		destinations[exit.destination] = stats
	}
	return destinations
}

func (self *reraceGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	return self.draw(nil, count, excludeDestinations, rankMode, false), nil
}

// the platform's family filters: a capable filter draws both of its
// categories, as find-providers2 does
func (self *reraceGenerator) NextDestinationsWithIpFamily(count int, excludeDestinations []MultiHopId, rankMode string, ipFamily IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	var families []IpFamily
	switch ipFamily {
	case IpFamilyFilterV6Capable:
		families = []IpFamily{IpFamilyDualstack, IpFamilyV6Only}
	default:
		families = []IpFamily{IpFamilyDualstack, IpFamilyV4Only}
	}
	return self.draw(families, count, excludeDestinations, rankMode, true), nil
}

func (self *reraceGenerator) NewClientArgsForDestination(destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, exit := range self.exits {
		if exit.destination == destination {
			args := &MultiClientGeneratorClientArgs{ClientId: NewId()}
			self.clientExits[args.ClientId] = exit
			return args, nil
		}
	}
	return nil, errors.New("unknown destination")
}

// RemoveClientArgs releases the provider for another channel.
func (self *reraceGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	self.mutex.Lock()
	unsub := self.unsubs[args.ClientId]
	delete(self.unsubs, args.ClientId)
	delete(self.clientExits, args.ClientId)
	self.mutex.Unlock()
	if unsub != nil {
		unsub()
	}
}

func (self *reraceGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
	self.RemoveClientArgs(args)
}

func (self *reraceGenerator) NewClientSettings() *ClientSettings {
	settings := DefaultClientSettingsWithBufferSize(testClientTransferBufferSize)
	settings.Log = NewNoopLogger()
	return settings
}

func (self *reraceGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	self.mutex.Lock()
	exit := self.clientExits[args.ClientId]
	self.mutex.Unlock()
	if exit == nil {
		return nil, errors.New("client args were not minted for a destination")
	}

	client := NewClient(ctx, args.ClientId, NewNoContractClientOob(), clientSettings)
	routeSend := make(chan []byte)
	routeReceive := make(chan []byte)
	transportSend := NewSendGatewayTransport()
	transportReceive := NewReceiveGatewayTransport()
	client.RouteManager().UpdateTransport(transportSend, []Route{routeSend})
	client.RouteManager().UpdateTransport(transportReceive, []Route{routeReceive})
	client.ContractManager().AddNoContractPeer(exit.provider.ClientId())

	providerTransportSend := NewSendClientTransport(DestinationId(args.ClientId))
	providerTransportReceive := NewReceiveGatewayTransport()
	exit.provider.RouteManager().UpdateTransport(providerTransportReceive, []Route{routeSend})
	exit.provider.RouteManager().UpdateTransport(providerTransportSend, []Route{routeReceive})
	exit.provider.ContractManager().AddNoContractPeer(client.ClientId())

	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.clients = append(self.clients, client)
	self.unsubs[args.ClientId] = func() {
		client.RouteManager().RemoveTransport(transportSend)
		client.RouteManager().RemoveTransport(transportReceive)
		exit.provider.RouteManager().RemoveTransport(providerTransportReceive)
		exit.provider.RouteManager().RemoveTransport(providerTransportSend)
	}
	return client, nil
}

func (self *reraceGenerator) clientList() []*Client {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return slices.Clone(self.clients)
}

// reraceAppPacket is one packet the multi client delivered to the app.
type reraceAppPacket struct {
	at     time.Time
	path   *IpPath
	packet []byte
}

// reraceHarness is a real multi client over test providers, seen from the
// app: packets go in through SendPacket and come back through the receive
// callback, recorded with the time they arrived.
type reraceHarness struct {
	t           *testing.T
	start       time.Time
	ctx         context.Context
	generator   *reraceGenerator
	multiClient *RemoteUserNatMultiClient
	log         *recordingLogger
	source      TransferPath
	exitWorkers sync.WaitGroup
	// concurrent is set while several goroutines drive the app: only one
	// goroutine may wait for the bubble to settle, so the app then polls on
	// synctest time alone, which advances only once the bubble is idle
	concurrent bool

	mutex    sync.Mutex
	received []reraceAppPacket
	synTimes map[string][]time.Time
}

// newReraceHarness starts a multi client with default settings (except as
// configure changes them) over one test provider per spec and waits until
// the quality window carries all of them. Must run inside a synctest bubble.
//
// It starts from a clean backend-degraded state and leaves one. The state is
// process-wide and stamped in clock time, and a degraded backend holds the
// window's size: failures an earlier test recorded -- every contract request
// on a NoContractClientOob is one, stamped in real time or in another
// bubble's -- must not decide whether this window forms.
func newReraceHarness(t *testing.T, specs []reraceExitSpec, configure func(*MultiClientSettings)) *reraceHarness {
	t.Helper()
	resetBackendDegraded()
	// registered first, so it runs last: after the clients below have closed
	t.Cleanup(resetBackendDegraded)
	ctx, cancel := context.WithCancel(t.Context())
	harness := &reraceHarness{
		t:         t,
		ctx:       ctx,
		generator: &reraceGenerator{clientExits: map[Id]*reraceExit{}, unsubs: map[Id]func(){}},
		log:       newRecordingLogger(),
		source:    SourceId(NewId()),
		synTimes:  map[string][]time.Time{},
	}
	for _, spec := range specs {
		harness.addExit(spec)
	}

	settings := DefaultMultiClientSettings()
	settings.Log = harness.log
	// the prober's unproven demerit would tie the exits' ranks as soon as one
	// of them carries traffic; selection by tier is what these tests steer
	settings.ProviderProbe = false
	settings.HeartbeatInterval = 0
	if configure != nil {
		configure(settings)
	}
	harness.multiClient = NewRemoteUserNatMultiClient(
		ctx,
		harness.generator,
		harness.receive,
		protocol.ProvideMode_Network,
		settings,
	)
	t.Cleanup(func() {
		if t.Failed() {
			lines := harness.log.lines()
			for _, line := range lines[max(0, len(lines)-200):] {
				t.Log(line)
			}
		}
		if err := harness.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Errorf("close multi client: %s", err)
		}
		cancel()
		for _, client := range harness.generator.clientList() {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("close channel client: %s", err)
			}
		}
		for _, exit := range harness.generator.exitList() {
			if err := exit.provider.CloseAndWait(context.Background()); err != nil {
				t.Errorf("close provider %s: %s", exit.name, err)
			}
		}
		harness.exitWorkers.Wait()
	})

	harness.waitForExits(len(specs))
	harness.start = time.Now()
	return harness
}

// addExit adds a provider the generator offers from now on.
func (self *reraceHarness) addExit(spec reraceExitSpec) *reraceExit {
	providerSettings := DefaultClientSettingsWithBufferSize(testClientTransferBufferSize)
	providerSettings.Log = NewNoopLogger()
	exit := &reraceExit{
		reraceExitSpec: spec,
		t:              self.t,
		provider:       NewClient(self.ctx, NewId(), NewNoContractClientOob(), providerSettings),
		replies:        make(chan reraceReply, 4096),
	}
	if exit.family == "" {
		exit.family = IpFamilyDualstack
	}
	exit.destination = RequireMultiHopId(exit.provider.ClientId())
	exit.provider.AddReceiveCallback(exit.receive)
	self.exitWorkers.Add(1)
	go func() {
		defer self.exitWorkers.Done()
		exit.run(self.ctx)
	}()
	self.generator.mutex.Lock()
	self.generator.exits = append(self.generator.exits, exit)
	self.generator.mutex.Unlock()
	return exit
}

func (self *reraceHarness) exit(name string) *reraceExit {
	self.t.Helper()
	for _, exit := range self.generator.exitList() {
		if exit.name == name {
			return exit
		}
	}
	self.t.Fatalf("no exit %q", name)
	return nil
}

// liveChannels is the quality window's channels that take new flows.
func (self *reraceHarness) liveChannels() []*multiClientChannel {
	channels := []*multiClientChannel{}
	for _, client := range self.multiClient.windows[WindowTypeQuality].unorderedClients() {
		if !client.IsDone() && !client.isWarning() {
			channels = append(channels, client)
		}
	}
	return channels
}

// waitForExits waits until the quality window holds n channels that take new
// flows.
func (self *reraceHarness) waitForExits(n int) {
	self.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for len(self.liveChannels()) < n {
		if deadline.Before(time.Now()) {
			self.t.Fatalf("the quality window holds %d live exits, want %d", len(self.liveChannels()), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// channel is the quality window's current channel to the named exit.
func (self *reraceHarness) channel(name string) *multiClientChannel {
	self.t.Helper()
	exit := self.exit(name)
	for _, client := range self.multiClient.windows[WindowTypeQuality].unorderedClients() {
		if client.Destination() == exit.destination && !client.IsDone() {
			return client
		}
	}
	self.t.Fatalf("no quality channel to %s", name)
	return nil
}

// inject has the named exit send packet to its quality channel, as a
// provider sends a packet of its own making, and lets it land.
func (self *reraceHarness) inject(name string, packet []byte) {
	self.t.Helper()
	self.exit(name).sendTo(self.channel(name).ClientId(), packet)
	self.settle()
}

func (self *reraceHarness) receive(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.received = append(self.received, reraceAppPacket{
		at:     time.Now(),
		path:   ipPath,
		packet: slices.Clone(packet),
	})
}

// delivered is every packet the app received for the flow, in order.
func (self *reraceHarness) delivered(path *IpPath) []reraceAppPacket {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	packets := []reraceAppPacket{}
	for _, packet := range self.received {
		if packet.path != nil && (reraceSameFlow(packet.path, path) || reraceSameFlow(packet.path, path.Reverse())) {
			packets = append(packets, packet)
		}
	}
	return packets
}

// errorsDelivered is every icmp error the app received for the flow.
func (self *reraceHarness) errorsDelivered(path *IpPath) []reraceAppPacket {
	errorPackets := []reraceAppPacket{}
	for _, packet := range self.delivered(path) {
		if _, ok := ipParseIcmpUnreachable(packet.packet); ok {
			errorPackets = append(errorPackets, packet)
		}
	}
	return errorPackets
}

// settle lets everything the last packet caused land.
func (self *reraceHarness) settle() {
	if !self.concurrent {
		synctest.Wait()
	}
}

// send hands one app packet to the multi client and lets it settle.
func (self *reraceHarness) send(packet []byte) bool {
	sent := self.multiClient.SendPacket(self.source, protocol.ProvideMode_Network, packet, reraceSendTimeout)
	self.settle()
	return sent
}

// reraceSendTimeout is how long the app's send may wait for an exit.
const reraceSendTimeout = 200 * time.Millisecond

// syn sends the flow's SYN once.
func (self *reraceHarness) syn(path *IpPath) bool {
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		key := reraceFlowKey(path)
		self.synTimes[key] = append(self.synTimes[key], time.Now())
	}()
	return self.send(reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil))
}

// sentSyns is when the app sent each of the flow's SYNs.
func (self *reraceHarness) sentSyns(path *IpPath) []time.Time {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return slices.Clone(self.synTimes[reraceFlowKey(path)])
}

// synAck is the first SYN-ACK the app received for the flow, if any.
func (self *reraceHarness) synAck(path *IpPath) (reraceAppPacket, bool) {
	for _, packet := range self.delivered(path) {
		if parsed, err := ParseIpPath(packet.packet); err == nil && parsed.Protocol == IpProtocolTcp && parsed.Syn && parsed.Ack {
			return packet, true
		}
	}
	return reraceAppPacket{}, false
}

// connect is the app's TCP connect: a SYN on path, retransmitted on
// reraceSynSchedule until a SYN-ACK arrives or the connect times out. It
// returns how many SYNs the app sent and when the SYN-ACK arrived, relative
// to the first SYN.
func (self *reraceHarness) connect(path *IpPath) (synCount int, elapsed time.Duration, ok bool) {
	start := time.Now()
	for _, offset := range reraceSynSchedule {
		if self.waitForSynAck(path, start.Add(offset)) {
			break
		}
		self.syn(path)
		synCount += 1
	}
	self.waitForSynAck(path, start.Add(reraceSynTimeout))
	synAck, ok := self.synAck(path)
	if !ok {
		return synCount, 0, false
	}
	return synCount, synAck.at.Sub(start), true
}

// waitForSynAck waits until the flow's SYN-ACK arrives or deadline passes.
func (self *reraceHarness) waitForSynAck(path *IpPath, deadline time.Time) bool {
	for {
		self.settle()
		if _, ok := self.synAck(path); ok {
			return true
		}
		wait := deadline.Sub(time.Now())
		if wait <= 0 {
			return false
		}
		time.Sleep(min(10*time.Millisecond, wait))
	}
}

// update is the multi client's flow state for path, nil when there is none.
func (self *reraceHarness) update(path *IpPath) *multiClientChannelUpdate {
	self.multiClient.stateLock.Lock()
	defer self.multiClient.stateLock.Unlock()
	switch path.Version {
	case 4:
		return self.multiClient.ip4PathUpdates[path.ToIp4Path()]
	default:
		return self.multiClient.ip6PathUpdates[path.ToIp6Path()]
	}
}

// boundTo is the channel the flow is bound to, nil when it is unbound or gone.
func (self *reraceHarness) boundTo(path *IpPath) *multiClientChannel {
	if update := self.update(path); update != nil {
		return update.client.Load()
	}
	return nil
}

func (self *reraceHarness) metrics() *ReliabilityMetricsSnapshot {
	return self.multiClient.ReliabilityMetrics()
}

// sleep advances synctest time with every goroutine settled at both ends.
func (self *reraceHarness) sleep(duration time.Duration) {
	synctest.Wait()
	time.Sleep(duration)
	synctest.Wait()
}

// reraceFlow is a fresh app flow to test destination destinationIndex.
func reraceFlow(version int, destinationIndex int, sourcePort int) *IpPath {
	path := &IpPath{
		Version:         version,
		Protocol:        IpProtocolTcp,
		SourcePort:      sourcePort,
		DestinationPort: 443,
		SequenceNumber:  uint32(100000 + sourcePort),
		Syn:             true,
	}
	switch version {
	case 4:
		path.SourceIp = net.IPv4(10, 1, 0, 2).To4()
		path.DestinationIp = net.IPv4(203, 0, 113, byte(10+destinationIndex)).To4()
	default:
		path.SourceIp = net.ParseIP("fd00:1::2")
		path.DestinationIp = net.ParseIP("2001:db8::")
		path.DestinationIp[15] = byte(10 + destinationIndex)
	}
	return path
}

// reraceUdpFlow is a fresh app quic flow to test destination destinationIndex.
func reraceUdpFlow(version int, destinationIndex int, sourcePort int) *IpPath {
	path := reraceFlow(version, destinationIndex, sourcePort)
	path.Protocol = IpProtocolUdp
	path.SequenceNumber = 0
	path.Syn = false
	return path
}

// reraceTcpPacket builds a tcp packet with explicit sequence and ack numbers.
func reraceTcpPacket(path *IpPath, flags byte, sequenceNumber uint32, ackNumber uint32, payload []byte) []byte {
	packet, tcp := ipTransportPacket(path, ipProtocolNumberTcp, TcpHeaderSizeWithoutExtensions+len(payload))
	binary.BigEndian.PutUint16(tcp[0:2], uint16(path.SourcePort))
	binary.BigEndian.PutUint16(tcp[2:4], uint16(path.DestinationPort))
	binary.BigEndian.PutUint32(tcp[4:8], sequenceNumber)
	binary.BigEndian.PutUint32(tcp[8:12], ackNumber)
	tcp[12] = byte(TcpHeaderSizeWithoutExtensions/4) << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	copy(tcp[TcpHeaderSizeWithoutExtensions:], payload)
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, tcp))
	return packet
}

// reraceSynAck is the destination's answer to the flow's SYN.
func reraceSynAck(path *IpPath) []byte {
	return reraceTcpPacket(path.Reverse(), tcpFlagSyn|tcpFlagAck, reraceServerIsn, path.SequenceNumber+1, nil)
}

// reraceSegment is an app data segment at offset on the flow's established
// connection.
func reraceSegment(path *IpPath, offset uint32, payload []byte) []byte {
	return reraceTcpPacket(path, tcpFlagAck|tcpFlagPsh, path.SequenceNumber+1+offset, reraceServerIsn+1, payload)
}

// reraceProviderFlowPath is the provider's path for the flow: the 5-tuple,
// with sequence zero.
func reraceProviderFlowPath(path *IpPath) *IpPath {
	return &IpPath{
		Version:         path.Version,
		Protocol:        path.Protocol,
		SourceIp:        path.SourceIp,
		SourcePort:      path.SourcePort,
		DestinationIp:   path.DestinationIp,
		DestinationPort: path.DestinationPort,
	}
}

// reraceProviderSignal is the packet a provider sends when its upstream dial
// for the flow fails at once: ipOosUnreachable over the provider's flow path.
func reraceProviderSignal(t *testing.T, path *IpPath) []byte {
	signal, ok := ipOosUnreachable(reraceProviderFlowPath(path))
	if !ok {
		t.Errorf("could not build the dial-failure signal for %+v", path)
	}
	return signal
}

func reraceFlowKey(path *IpPath) string {
	return fmt.Sprintf("%d/%s/%s/%d/%s/%d", path.Version, path.Protocol, path.SourceIp, path.SourcePort, path.DestinationIp, path.DestinationPort)
}

// reraceSameFlow compares the 5-tuples of two paths in the same direction.
func reraceSameFlow(a *IpPath, b *IpPath) bool {
	return a.Version == b.Version && a.Protocol == b.Protocol &&
		a.SourceIp.Equal(b.SourceIp) && a.SourcePort == b.SourcePort &&
		a.DestinationIp.Equal(b.DestinationIp) && a.DestinationPort == b.DestinationPort
}

// reraceAssertNoErrorDelivered fails if the app received an icmp error for
// the flow.
func reraceAssertNoErrorDelivered(t *testing.T, harness *reraceHarness, path *IpPath) {
	t.Helper()
	for _, packet := range harness.errorsDelivered(path) {
		t.Errorf("an icmp error reached the app at +%s", packet.at.Sub(harness.start))
	}
}

// The harness forms its window whatever backend failures earlier tests left
// in the process-wide degraded state, which holds a window's size: failures
// stamped in real time, which a synctest bubble's clock (it starts in 2000)
// reads as after now, and failures stamped in a bubble's own time, which a
// later bubble's clock reaches. It leaves the state clean behind it.
func TestReraceHarnessIgnoresEarlierBackendFailures(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()
	GetMessagePoolAggregateStats()
	specs := []reraceExitSpec{
		{name: "a", tier: 0},
		{name: "b", tier: 1},
	}
	t.Run("real time", func(t *testing.T) {
		for range backendDegradedFailThreshold {
			noteBackendFailure()
		}
		synctest.Test(t, func(t *testing.T) {
			newReraceHarness(t, specs, nil)
		})
	})
	t.Run("bubble time", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			for range backendDegradedFailThreshold {
				noteBackendFailure()
			}
			newReraceHarness(t, specs, nil)
		})
	})
	t.Run("left behind", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			newReraceHarness(t, specs, nil)
			// failures the harness's own clients record in bubble time
			for range backendDegradedFailThreshold {
				noteBackendFailure()
			}
		})
	})
	if got, last := consecutiveBackendFails.Load(), lastBackendFailNano.Load(); got != 0 || last != 0 {
		t.Errorf("the harness left %d backend failures behind, the last stamped %d", got, last)
	}
}

// --- RR-1: the re-race ---

// RR-1. A new flow whose exit answers with the dial-failure signal is unbound
// and its next retransmit completes on another exit within one retransmit
// interval -- not at the 3s inference, not on the app's backoff. The signal
// never reaches the app. The first SYN goes to A alone because A is the only
// exit of the best tier, as a pinned or single-candidate flow does; the
// retransmit crosses tiers because A has failed this destination service.
// Quic (udp 443) gets the same, on the provider's udp signal.
func TestDialFailureReraceRetransmitCompletesOnAnotherExit(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("tcp/ipv%d", version), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				harness := newReraceHarness(t, []reraceExitSpec{
					{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
					{name: "b", tier: 1},
				}, nil)
				a, b := harness.exit("a"), harness.exit("b")
				path := reraceFlow(version, 0, 40001)

				synCount, elapsed, ok := harness.connect(path)
				if !ok {
					t.Fatalf("the connect never completed: A saw %d SYNs, B saw %d", len(a.syns(path)), len(b.syns(path)))
				}
				if synCount != 2 {
					t.Errorf("the app sent %d SYNs, want 2: the first retransmit must complete", synCount)
				}
				// the first retransmit leaves at 1s and the SYN-ACK it draws
				// is the recovery: one retransmit interval
				if elapsed < reraceSynSchedule[1] || reraceSynSchedule[1]+reraceSettle < elapsed {
					t.Errorf("the SYN-ACK arrived %s after the first SYN, want at the first retransmit (%s)", elapsed, reraceSynSchedule[1])
				}
				if got := len(a.syns(path)); got != 1 {
					t.Errorf("A received %d SYNs, want 1: the retransmit must leave the failed exit", got)
				}
				if got := len(b.syns(path)); got != 1 {
					t.Errorf("B received %d SYNs, want exactly the retransmit", got)
				}
				if got := len(harness.delivered(path)); got != 1 {
					t.Errorf("the app received %d packets, want only the SYN-ACK", got)
				}
				reraceAssertNoErrorDelivered(t, harness, path)
				if harness.boundTo(path) != harness.channel("b") {
					t.Error("the flow is not bound to B")
				}
				metrics := harness.metrics()
				if metrics.DialFailuresIntercepted != 1 || metrics.FlowsReraced != 1 {
					t.Errorf("DialFailuresIntercepted=%d FlowsReraced=%d, want 1 and 1", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
				}
				if got := harness.channel("a").dialFailureCount(); got != 1 {
					t.Errorf("A dialFailureCount = %d, want 1", got)
				}
			})
		})
		t.Run(fmt.Sprintf("quic/ipv%d", version), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				harness := newReraceHarness(t, []reraceExitSpec{
					{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
					{name: "b", tier: 1},
				}, nil)
				a, b := harness.exit("a"), harness.exit("b")
				path := reraceUdpFlow(version, 0, 40001)
				initial := ipOosUdpPacket(path, []byte("quic initial"))

				// the initial, and one probe timeout later its retransmit
				harness.send(slices.Clone(initial))
				if got := len(harness.delivered(path)); got != 0 {
					t.Fatalf("the app received %d packets after the failed initial, want none", got)
				}
				if harness.boundTo(path) != nil {
					t.Fatal("the quic flow was not unbound by the signal")
				}
				harness.sleep(time.Second)
				harness.send(slices.Clone(initial))

				if got := len(a.received(path)); got != 1 {
					t.Errorf("A received %d datagrams, want 1", got)
				}
				if got := len(b.received(path)); got != 1 {
					t.Errorf("B received %d datagrams, want the retransmit", got)
				}
				delivered := harness.delivered(path)
				if len(delivered) != 1 {
					t.Fatalf("the app received %d packets, want only B's answer", len(delivered))
				}
				if parsed, err := ParseIpPath(delivered[0].packet); err != nil || parsed.Protocol != IpProtocolUdp {
					t.Errorf("the app received %x, want B's datagram", delivered[0].packet)
				}
				if harness.boundTo(path) != harness.channel("b") {
					t.Error("the quic flow is not bound to B")
				}
				if metrics := harness.metrics(); metrics.FlowsReraced != 1 {
					t.Errorf("FlowsReraced = %d, want 1", metrics.FlowsReraced)
				}
			})
		})
	}
}

// RR-1. A provider whose dial fails at once can answer before a race commits
// the flow: the send returns once the SYN is queued, and the exit's signal
// may already have landed. The exit is then taken out of the race, with its
// strike, so the one-candidate race cannot commit the flow to the exit that
// failed it, and the app's next SYN goes to another exit and completes. It
// counts as the re-race it would have been had the commit come first. The
// send hook delivers the signal before the send returns, deterministically.
func TestDialFailureSignalBeforeTheCommitWithdrawsTheExit(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
			{name: "b", tier: 1},
		}, nil)
		a := harness.channel("a")
		path := reraceFlow(4, 0, 47000)
		a.sendGroupForTest = reraceSignalDuringSend(t, harness, a, path)
		harness.syn(path)
		a.sendGroupForTest = nil
		if got := harness.metrics().DialFailuresIntercepted; got != 1 {
			t.Fatalf("setup: DialFailuresIntercepted = %d, want the signal", got)
		}
		if harness.boundTo(path) == a {
			t.Fatal("the signal that landed before the commit was dropped: the flow is bound to the exit that failed it")
		}
		if got := a.dialFailureCount(); got != 1 {
			t.Errorf("A carries %d strikes, want the signal's 1", got)
		}
		harness.sleep(time.Second)
		harness.syn(path)
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != harness.channel("b") {
			t.Error("the next SYN did not complete on B")
		}
		// whether the signal beats the commit or follows it, the flow leaves
		// the exit that failed it once, and counts once
		if got := harness.metrics().FlowsReraced; got != 1 {
			t.Errorf("FlowsReraced = %d, want 1, as when the commit comes first", got)
		}
	})
}

// RR-1. In a race of several exits, a candidate whose signal lands is taken
// out of the race, with its strike, and the race goes on: the other candidate
// answers and wins.
func TestDialFailureRaceCandidateSignalLeavesTheRaceToTheOthers(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
			{name: "b", tier: 0},
		}, nil)
		a, b := harness.channel("a"), harness.channel("b")
		path := reraceFlow(4, 0, 47001)
		// A's signal lands during its send; B answers once it has
		signaled := make(chan struct{})
		var signaledOnce sync.Once
		signal := reraceSignalDuringSend(t, harness, a, path)
		a.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
			defer signaledOnce.Do(func() { close(signaled) })
			return signal(group, timeout, ack)
		}
		b.sendGroupForTest = reraceAnswerDuringSend(t, harness, b, reraceSynAck(path), signaled)
		synCount, _, ok := harness.connect(path)
		a.sendGroupForTest, b.sendGroupForTest = nil, nil
		if !ok || synCount != 1 {
			t.Fatalf("the connect completed on SYN %d (%t), want on the first", synCount, ok)
		}
		if harness.boundTo(path) != b {
			t.Error("the flow is not on B")
		}
		if got := a.dialFailureCount(); got != 1 {
			t.Errorf("A carries %d strikes, want 1", got)
		}
		if got := harness.metrics().FlowsReraced; got != 0 {
			t.Errorf("FlowsReraced = %d, want 0: a race candidate's signal unbinds nothing", got)
		}
	})
}

// RR-1. A race whose every candidate signals is left empty and ends, so the
// flow's next SYN starts a race of its own, which completes on another exit.
func TestDialFailureWithdrawnRaceStartsAfresh(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
			{name: "b", tier: 0, answer: reraceAlways(reraceSignal)},
			{name: "c", tier: 1},
			{name: "d", tier: 1},
		}, nil)
		a, b := harness.channel("a"), harness.channel("b")
		path := reraceFlow(4, 0, 47002)
		a.sendGroupForTest = reraceSignalDuringSend(t, harness, a, path)
		b.sendGroupForTest = reraceSignalDuringSend(t, harness, b, path)
		synCount, _, ok := harness.connect(path)
		a.sendGroupForTest, b.sendGroupForTest = nil, nil
		if !ok || synCount != 2 {
			t.Fatalf("the connect completed on SYN %d (%t), want on the second", synCount, ok)
		}
		if bound := harness.boundTo(path); bound != harness.channel("c") && bound != harness.channel("d") {
			t.Error("the flow is not on C or D")
		}
		for _, channel := range []*multiClientChannel{a, b} {
			if got := channel.dialFailureCount(); got != 1 {
				t.Errorf("an exit that failed the race carries %d strikes, want 1", got)
			}
		}
	})
}

// RR-1. An emptied race ends rather than lingering for the next SYN: when that
// SYN finds a single exit, it gets a race of one of its own, which commits on
// the send, so that exit's signal before the commit counts as the re-race it
// is, as for a race of one from the start.
func TestDialFailureEmptiedRaceGivesTheNextSynARaceOfItsOwn(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
			{name: "b", tier: 0, answer: reraceAlways(reraceSignal)},
			{name: "c", tier: 1, answer: reraceAlways(reraceSignal)},
		}, nil)
		a, b, c := harness.channel("a"), harness.channel("b"), harness.channel("c")
		path := reraceFlow(4, 0, 47003)
		for _, channel := range []*multiClientChannel{a, b, c} {
			channel.sendGroupForTest = reraceSignalDuringSend(t, harness, channel, path)
		}
		// the first SYN races A and B, which both fail it
		harness.syn(path)
		if got := harness.metrics().FlowsReraced; got != 0 {
			t.Fatalf("FlowsReraced = %d after the race of two, want 0", got)
		}
		// the retransmit finds only C, which fails it before the commit
		harness.sleep(time.Second)
		harness.syn(path)
		for _, channel := range []*multiClientChannel{a, b, c} {
			channel.sendGroupForTest = nil
		}
		if got := harness.metrics().DialFailuresIntercepted; got != 3 {
			t.Fatalf("setup: DialFailuresIntercepted = %d, want the three signals", got)
		}
		if got := harness.metrics().FlowsReraced; got != 1 {
			t.Errorf("FlowsReraced = %d, want C's 1: its race of one was its own", got)
		}
		if harness.boundTo(path) != nil {
			t.Error("the flow is bound to an exit that failed it")
		}
	})
}

// reraceSignalDuringSend is a send hook for an exit's channel: the exit's
// provider fails the flow's dial at once, and its signal lands before the
// send returns.
func reraceSignalDuringSend(t *testing.T, harness *reraceHarness, channel *multiClientChannel, path *IpPath) func(*parsedPacketGroup, time.Duration, bool) (bool, error) {
	t.Helper()
	return reraceAnswerDuringSend(t, harness, channel, reraceProviderSignal(t, path), nil)
}

// reraceAnswerDuringSend is a send hook for an exit's channel: the exit's
// provider answers the sent packet with answer before the send returns,
// once after is closed when it is not nil.
func reraceAnswerDuringSend(t *testing.T, harness *reraceHarness, channel *multiClientChannel, answer []byte, after <-chan struct{}) func(*parsedPacketGroup, time.Duration, bool) (bool, error) {
	t.Helper()
	var provider Id
	for _, exit := range harness.generator.exitList() {
		if exit.destination == channel.Destination() {
			provider = exit.provider.ClientId()
		}
	}
	if provider == (Id{}) {
		t.Fatal("no provider behind the channel")
	}
	return func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
		if after != nil {
			<-after
		}
		frame, err := ipPacketFromProviderFrame(answer, DefaultProtocolVersion)
		if err != nil {
			t.Errorf("answer frame: %s", err)
		} else {
			channel.clientReceive(SourceId(provider), []*protocol.Frame{frame}, Peer{})
		}
		for packetIndex := range group.packets {
			MessagePoolReturn(group.packets[packetIndex].packet)
		}
		return true, nil
	}
}

// --- RR-2: established flows ---

// RR-2. A flow that has received inbound data is never disturbed by a
// signal: it stays on its exit and keeps carrying data, no strike is
// recorded and nothing reaches the app. The signal is still counted.
func TestDialFailureSignalNeverDisturbsEstablishedFlow(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				harness := newReraceHarness(t, []reraceExitSpec{
					{name: "a", tier: 0},
					{name: "b", tier: 1},
				}, nil)
				path := reraceFlow(version, 0, 40002)
				if _, _, ok := harness.connect(path); !ok {
					t.Fatal("the connect through A did not complete")
				}
				a := harness.channel("a")
				update := harness.update(path)
				if update.client.Load() != a || !update.receivedInbound.Load() {
					t.Fatal("setup: the flow is not established on A")
				}
				harness.send(reraceSegment(path, 0, []byte("request")))

				harness.inject("a", reraceProviderSignal(t, path))

				if update.client.Load() != a || harness.update(path) != update {
					t.Fatal("a signal moved an established flow")
				}
				if got := a.dialFailureCount(); got != 0 {
					t.Errorf("A dialFailureCount = %d, want 0", got)
				}
				metrics := harness.metrics()
				if metrics.DialFailuresIntercepted != 1 || metrics.FlowsReraced != 0 {
					t.Errorf("DialFailuresIntercepted=%d FlowsReraced=%d, want 1 and 0", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
				}
				reraceAssertNoErrorDelivered(t, harness, path)

				// the flow still carries data both ways through A
				harness.send(reraceSegment(path, 7, []byte("second")))
				segments := 0
				for _, packet := range harness.exit("a").received(path) {
					if 0 < len(packet.payload) {
						segments += 1
					}
				}
				if segments != 2 {
					t.Errorf("A received %d data segments, want 2", segments)
				}
				if got := len(harness.delivered(path)); got != 3 {
					t.Errorf("the app received %d packets, want the SYN-ACK and two echoes", got)
				}
				if got := len(harness.exit("b").received(path)); got != 0 {
					t.Errorf("B received %d packets of the flow, want 0", got)
				}
			})
		})
	}
}

// --- RR-3: stale signals ---

// RR-3. A stale signal is ignored: after the flow moved to another exit,
// after the flow is gone, from an exit that never owned it, and a repeat of
// the signal that already unbound it. None of them moves a flow, strikes an
// exit or reaches the app; each is counted as intercepted.
func TestDialFailureStaleSignalIsIgnored(t *testing.T) {
	GetMessagePoolAggregateStats()

	t.Run("after the flow moved to another exit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			harness := newReraceHarness(t, []reraceExitSpec{
				{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
				{name: "b", tier: 1, answer: reraceAlways(reraceSilent)},
			}, nil)
			path := reraceFlow(4, 0, 40003)
			harness.syn(path)
			harness.sleep(time.Second)
			harness.syn(path)
			b := harness.channel("b")
			if harness.boundTo(path) != b {
				t.Fatal("setup: the retransmit did not move the flow to B")
			}

			harness.inject("a", reraceProviderSignal(t, path))

			if harness.boundTo(path) != b {
				t.Error("A's late signal moved the flow off B")
			}
			if got := harness.channel("a").dialFailureCount(); got != 1 {
				t.Errorf("A dialFailureCount = %d, want the first strike only", got)
			}
			if got := b.dialFailureCount(); got != 0 {
				t.Errorf("B dialFailureCount = %d, want 0", got)
			}
			metrics := harness.metrics()
			if metrics.DialFailuresIntercepted != 2 || metrics.FlowsReraced != 1 {
				t.Errorf("DialFailuresIntercepted=%d FlowsReraced=%d, want 2 and 1", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
			}
			if got := len(harness.delivered(path)); got != 0 {
				t.Errorf("the app received %d packets, want none", got)
			}
		})
	})

	t.Run("after the flow is gone", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			harness := newReraceHarness(t, []reraceExitSpec{
				{name: "a", tier: 0},
				{name: "b", tier: 1},
			}, nil)
			path := reraceFlow(4, 0, 40004)
			if _, _, ok := harness.connect(path); !ok {
				t.Fatal("setup: the connect did not complete")
			}
			// the app resets the connection, and the flow is retired
			harness.send(reraceTcpPacket(path, tcpFlagRst|tcpFlagAck, path.SequenceNumber+1, reraceServerIsn+1, nil))
			harness.sleep(time.Second)
			if harness.update(path) != nil {
				t.Fatal("setup: the reset flow was not retired")
			}
			before := len(harness.delivered(path))

			harness.inject("a", reraceProviderSignal(t, path))

			if harness.update(path) != nil {
				t.Error("a signal for a gone flow brought it back")
			}
			if got := harness.channel("a").dialFailureCount(); got != 0 {
				t.Errorf("A dialFailureCount = %d, want 0", got)
			}
			metrics := harness.metrics()
			if metrics.DialFailuresIntercepted != 1 || metrics.FlowsReraced != 0 {
				t.Errorf("DialFailuresIntercepted=%d FlowsReraced=%d, want 1 and 0", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
			}
			if got := len(harness.delivered(path)); got != before {
				t.Errorf("the app received %d more packets, want none", got-before)
			}
		})
	})

	t.Run("from an exit that does not own the flow", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			harness := newReraceHarness(t, []reraceExitSpec{
				{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
				{name: "b", tier: 1},
			}, nil)
			path := reraceFlow(4, 0, 40005)
			harness.syn(path)
			a := harness.channel("a")
			if harness.boundTo(path) != a {
				t.Fatal("setup: the SYN did not bind the flow to A")
			}

			harness.inject("b", reraceProviderSignal(t, path))

			if harness.boundTo(path) != a {
				t.Error("B's signal unbound A's flow")
			}
			if got := harness.channel("b").dialFailureCount(); got != 0 {
				t.Errorf("B dialFailureCount = %d, want 0", got)
			}
			if got := a.dialFailureCount(); got != 0 {
				t.Errorf("A dialFailureCount = %d, want 0", got)
			}
			metrics := harness.metrics()
			if metrics.DialFailuresIntercepted != 1 || metrics.FlowsReraced != 0 {
				t.Errorf("DialFailuresIntercepted=%d FlowsReraced=%d, want 1 and 0", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
			}
			if got := len(harness.delivered(path)); got != 0 {
				t.Errorf("the app received %d packets, want none", got)
			}

			// positive control: the owner's signal does unbind
			harness.inject("a", reraceProviderSignal(t, path))
			if harness.boundTo(path) != nil {
				t.Error("A's own signal did not unbind the flow")
			}
		})
	})

	t.Run("repeated after the unbind", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			harness := newReraceHarness(t, []reraceExitSpec{
				{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
				{name: "b", tier: 1},
			}, nil)
			path := reraceFlow(4, 0, 40006)
			harness.syn(path)
			if harness.boundTo(path) != nil {
				t.Fatal("setup: the signal did not unbind the flow")
			}

			harness.inject("a", reraceProviderSignal(t, path))

			if got := harness.channel("a").dialFailureCount(); got != 1 {
				t.Errorf("A dialFailureCount = %d, want 1: one failed SYN is one strike", got)
			}
			metrics := harness.metrics()
			if metrics.DialFailuresIntercepted != 2 || metrics.FlowsReraced != 1 {
				t.Errorf("DialFailuresIntercepted=%d FlowsReraced=%d, want 2 and 1", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
			}
			if got := len(harness.delivered(path)); got != 0 {
				t.Errorf("the app received %d packets, want none", got)
			}
		})
	})
}

// --- RR-4: probes ---

// RR-4. A dial-failure signal for a probe flow, arriving through the
// channel's own intercept, answers the probe and feeds nothing: no strike,
// no re-race, no metric, no uplink stamp and nothing to the app, in both
// positions of the knob. A signal naming a probe address after the probe
// ended is consumed the same way.
func TestProbeDialFailureThroughChannelFeedsNothing(t *testing.T) {
	for _, rerace := range []bool{true, false} {
		t.Run(fmt.Sprintf("rerace=%t", rerace), func(t *testing.T) {
			parent, client, forwarded := probeTestParent(t)
			parent.settings.DialFailureRerace = rerace
			client.dialFailureCallback = parent.clientDialFailureSignal
			client.clientReceivePacketCallback = parent.clientReceivePacket

			resultCh := make(chan probeResult, 1)
			go func() {
				resultCh <- parent.probeExit(client, []probeTarget{probeTestTarget()}, 30*time.Second, 0)
			}()
			update := waitForProbeFlow(t, parent)
			signal := reraceProviderSignal(t, update.probe.ipPath)
			deliver := func() {
				client.clientReceive(TransferPath{}, []*protocol.Frame{
					RequireToFrameWithDefaultProtocolVersion(&protocol.IpPacketFromProvider{
						IpPacket: &protocol.IpPacket{PacketBytes: signal},
					}),
				}, Peer{ProvideMode: protocol.ProvideMode_Network})
			}

			deliver()
			select {
			case result := <-resultCh:
				if result.Answered != 0 || result.Passed {
					t.Errorf("the signal passed the probe: %+v", result)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the signal did not end the probe")
			}
			// the probe has ended; the same signal again names no live probe
			deliver()

			if got := client.dialFailureCount(); got != 0 {
				t.Errorf("dialFailureCount = %d, want 0", got)
			}
			if metrics := parent.ReliabilityMetrics(); metrics.DialFailuresIntercepted != 0 || metrics.FlowsReraced != 0 {
				t.Errorf("DialFailuresIntercepted=%d FlowsReraced=%d, want 0 and 0", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
			}
			if got := parent.uplinkLastIngressNanos.Load(); got != 0 {
				t.Errorf("the uplink was stamped at %d by a probe's failure", got)
			}
			if got := len(*forwarded); got != 0 {
				t.Errorf("%d packets reached the app", got)
			}
			if stats, err := client.WindowStats(); err != nil || stats.receiveAckCount != 0 {
				t.Errorf("the signal counted as received data: %v", err)
			}
		})
	}
}

// --- RR-5: the knob off ---

// RR-5. With DialFailureRerace off the signal is handed to the app at once,
// once per failed dial, rebuilt over the SYN's sequence so a kernel stack
// accepts it, and nothing re-races: through the retransmits at 1s,
// 3s and 7s -- the 3s and 7s ones past the silence inference, which the knob
// turns off too -- the flow stays on its exit and the other exit never sees
// it. (Blackhole detection benches an exit that never answers later; that is
// not a re-race.)
func TestDialFailureReraceOffForwardsSignalAndKeepsExit(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				harness := newReraceHarness(t, []reraceExitSpec{
					{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
					{name: "b", tier: 1},
				}, func(settings *MultiClientSettings) {
					settings.DialFailureRerace = false
				})
				a := harness.channel("a")
				path := reraceFlow(version, 0, 40007)
				start := time.Now()
				for _, offset := range reraceSynSchedule[:4] {
					time.Sleep(start.Add(offset).Sub(time.Now()))
					harness.syn(path)
					if harness.boundTo(path) != a {
						t.Fatalf("the flow left A at +%s with the knob off", offset)
					}
				}

				errorsDelivered := harness.errorsDelivered(path)
				failedDials := harness.exit("a").syns(path)
				if len(errorsDelivered) == 0 || len(errorsDelivered) != len(failedDials) {
					t.Fatalf("the app received %d icmp errors for %d failed dials, want one each", len(errorsDelivered), len(failedDials))
				}
				for i, packet := range errorsDelivered {
					if packet.at.Sub(failedDials[i].at) != 0 {
						t.Errorf("icmp error %d reached the app %s after its failed dial, want at once", i, packet.at.Sub(failedDials[i].at))
					}
					embedded, _ := ipParseIcmpUnreachable(packet.packet)
					if !reraceSameFlow(embedded, path) {
						t.Errorf("icmp error %d names %+v, want the flow", i, embedded)
					}
					// the provider's signal embeds sequence 0; the app's stack
					// accepts only its SYN's
					if embedded.SequenceNumber != path.SequenceNumber {
						t.Errorf("icmp error %d embeds sequence %d, want the SYN's %d", i, embedded.SequenceNumber, path.SequenceNumber)
					}
					want := reraceProviderFlowPath(path)
					want.SequenceNumber = path.SequenceNumber
					if wantPacket, _ := ipOosUnreachable(want); !bytes.Equal(packet.packet, wantPacket) {
						t.Errorf("icmp error %d is %x, want %x", i, packet.packet, wantPacket)
					}
				}
				if got := len(harness.delivered(path)); got != len(errorsDelivered) {
					t.Errorf("the app received %d packets, want only the %d icmp errors", got, len(errorsDelivered))
				}
				if got := len(harness.exit("b").received(path)); got != 0 {
					t.Errorf("B received %d packets of the flow, want 0", got)
				}
				metrics := harness.metrics()
				if metrics.FlowsReraced != 0 || metrics.DialFailuresIntercepted != uint64(len(failedDials)) {
					t.Errorf("FlowsReraced=%d DialFailuresIntercepted=%d, want 0 and %d", metrics.FlowsReraced, metrics.DialFailuresIntercepted, len(failedDials))
				}
			})
		})
	}
}

// --- RR-6: strikes ---

// RR-6. Strikes recorded from real signals through the channel: failures
// for one destination, however many, never make an exit dial-starved;
// failures spanning two destinations with no success in the window do; a
// proven connect on the exit clears it.
func TestDialStarvedFromSignalsAcrossDestinations(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		// on A, destinations 1 and 2 fail and destination 3 is slow
		// (answered later, by hand, so A proves no connect on its own); B is
		// two tiers down so every first SYN tries A
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: func(path *IpPath) reraceAnswer {
				switch path.DestinationIp[len(path.DestinationIp)-1] {
				case 11, 12:
					return reraceSignal
				default:
					return reraceSilent
				}
			}},
			{name: "b", tier: 2},
		}, nil)
		a := harness.channel("a")
		slow := reraceFlow(4, 3, 41000)
		harness.syn(slow)
		if harness.boundTo(slow) != a {
			t.Fatal("setup: the slow flow is not on A")
		}

		// four failures, all for destination 1: one port each, since a
		// failed destination service is not offered to A again
		for i, port := range []int{443, 8443, 80, 8080} {
			path := reraceFlow(4, 1, 41001+i)
			path.DestinationPort = port
			if _, _, ok := harness.connect(path); !ok {
				t.Fatalf("flow %d to destination 1 did not complete on B", i)
			}
			if got := len(harness.exit("a").syns(path)); got != 1 {
				t.Fatalf("flow %d to destination 1: A received %d SYNs, want 1", i, got)
			}
		}
		if got := a.dialFailureCount(); got != 4 {
			t.Fatalf("A dialFailureCount = %d, want 4", got)
		}
		if a.dialStarved() {
			t.Error("strikes for one destination made A dial-starved")
		}

		// one failure for a second destination
		second := reraceFlow(4, 2, 41011)
		if _, _, ok := harness.connect(second); !ok {
			t.Fatal("the flow to destination 2 did not complete on B")
		}
		if got := len(harness.exit("a").syns(second)); got != 1 {
			t.Fatalf("the flow to destination 2: A received %d SYNs, want 1", got)
		}
		if !a.dialStarved() {
			t.Fatal("strikes across two destinations with no success did not make A dial-starved")
		}

		// A proves a connect: the slow flow's SYN-ACK lands
		harness.inject("a", reraceSynAck(slow))
		if _, ok := harness.synAck(slow); !ok {
			t.Fatal("the slow flow's SYN-ACK did not reach the app")
		}
		if a.dialStarved() {
			t.Error("a proven connect did not clear dial starvation")
		}
		if got := a.dialFailureCount(); got != 5 {
			t.Errorf("A dialFailureCount = %d, want the 5 strikes still in the window", got)
		}
	})
}

// RR-6. Strikes age out of the window together with their destinations:
// three surviving strikes for one destination are not starvation even though
// the aged strikes spanned two, and a fresh strike for the second
// destination makes the span again.
func TestDialStarvedStrikesAgeOutWithTheirDestinations(t *testing.T) {
	client := &multiClientChannel{settings: DefaultMultiClientSettings()}
	client.addDialFailure("93.184.216.34", 4)
	client.addDialFailure("142.250.74.100", 4)
	client.addDialFailure("93.184.216.34", 4)
	if !client.dialStarved() {
		t.Fatal("setup: three strikes across two destinations are not starvation")
	}
	// every strike so far leaves the window
	old := time.Now().Add(-dialStrikeWindow - time.Second)
	for i := range client.dialFailureTimes {
		client.dialFailureTimes[i] = old
	}
	if client.dialStarved() || client.dialFailureCount() != 0 {
		t.Fatalf("aged strikes still count: starved=%t count=%d", client.dialStarved(), client.dialFailureCount())
	}
	if len(client.dialFailureDestinations) != 0 || len(client.dialFailureVersions) != 0 {
		t.Fatalf("destinations %v and versions %v outlived their strikes", client.dialFailureDestinations, client.dialFailureVersions)
	}

	// two old strikes span both destinations; the survivors name one
	client.addDialFailure("93.184.216.34", 4)
	client.addDialFailure("142.250.74.100", 4)
	client.dialFailureTimes[0] = old
	client.dialFailureTimes[1] = old
	client.addDialFailure("93.184.216.34", 4)
	client.addDialFailure("93.184.216.34", 4)
	client.addDialFailure("93.184.216.34", 4)
	if client.dialStarved() {
		t.Error("an aged strike's destination still made the span")
	}
	if got := client.dialFailureCount(); got != 3 {
		t.Errorf("dialFailureCount = %d, want the 3 surviving strikes", got)
	}
	client.addDialFailure("142.250.74.100", 4)
	if !client.dialStarved() {
		t.Error("a fresh strike for the second destination did not make the span")
	}
}

// RR-6. A proven connect clears starvation only while it is in the window:
// once it ages out, the strikes that outlive it convict again.
func TestDialStarvedConnectSuccessAgesOut(t *testing.T) {
	client := &multiClientChannel{settings: DefaultMultiClientSettings()}
	client.addConnectSuccess(4)
	client.addDialFailure("93.184.216.34", 4)
	client.addDialFailure("142.250.74.100", 4)
	client.addDialFailure("93.184.216.34", 4)
	if client.dialStarved() {
		t.Fatal("strikes beside a proven connect in the window read as starvation")
	}
	client.connectSuccessTimes[0] = time.Now().Add(-dialStrikeWindow - time.Second)
	if !client.dialStarved() {
		t.Error("an aged connect success still cleared starvation")
	}
	if len(client.connectSuccessTimes) != 0 || len(client.connectSuccessVersions) != 0 {
		t.Error("the aged success was not pruned with its version")
	}
}

// RR-6. An exit strikes a flow once, however often it fails it. A provider
// whose udp socket open fails at once answers every datagram with its signal,
// and a flow with one candidate exit re-races straight back onto it; a tcp
// flow does the same on every retransmit. Each repeat is counted as a signal
// but not as a strike or a re-race, so the strikes grow with the flows, not
// the packet rate.
func TestDialFailureStrikesOncePerFlowAndExit(t *testing.T) {
	GetMessagePoolAggregateStats()
	t.Run("udp datagrams", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			harness := newReraceHarness(t, []reraceExitSpec{
				{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
			}, nil)
			a := harness.channel("a")
			path := reraceUdpFlow(4, 1, 46000)
			// an rtp stream, not a watched handshake port
			path.DestinationPort = 5004
			for range 100 {
				harness.send(ipOosUdpPacket(path, []byte("rtp")))
				harness.sleep(20 * time.Millisecond)
			}
			datagrams := len(harness.exit("a").received(path))
			if datagrams != 100 {
				t.Errorf("A received %d of the flow's 100 datagrams", datagrams)
			}
			if got := a.dialFailureCount(); got != 1 {
				t.Errorf("one udp flow left %d strikes on A, want 1", got)
			}
			metrics := harness.metrics()
			if metrics.FlowsReraced != 1 || metrics.DialFailuresIntercepted != uint64(datagrams) {
				t.Errorf("FlowsReraced=%d DialFailuresIntercepted=%d, want 1 and one per datagram (%d)", metrics.FlowsReraced, metrics.DialFailuresIntercepted, datagrams)
			}
			if got := len(harness.delivered(path)); got != 0 {
				t.Errorf("the app received %d packets, want none", got)
			}
		})
	})
	t.Run("tcp retransmits", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			harness := newReraceHarness(t, []reraceExitSpec{
				{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
			}, nil)
			a := harness.channel("a")
			path := reraceFlow(4, 1, 46001)
			// the SYNs inside the strike window, before blackhole detection
			// benches an exit that has received nothing
			start := time.Now()
			for _, offset := range reraceSynSchedule[:4] {
				time.Sleep(start.Add(offset).Sub(time.Now()))
				harness.syn(path)
			}
			syns := len(harness.exit("a").syns(path))
			if syns != 4 {
				t.Errorf("A received %d of the app's 4 SYNs", syns)
			}
			if got := a.dialFailureCount(); got != 1 {
				t.Errorf("one tcp flow left %d strikes on A over 4 SYNs, want 1", got)
			}
			metrics := harness.metrics()
			if metrics.FlowsReraced != 1 || metrics.DialFailuresIntercepted != uint64(syns) {
				t.Errorf("FlowsReraced=%d DialFailuresIntercepted=%d, want 1 and one per SYN (%d)", metrics.FlowsReraced, metrics.DialFailuresIntercepted, syns)
			}
		})
	})
}

// RR-6. An exit strikes each connect attempt of a flow once: a SYN with a new
// sequence on the same 5-tuple starts a new attempt, which the exit that
// failed the old one may serve, and strike, again.
func TestDialFailureNewSynGenerationStrikesAgain(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
		}, nil)
		a := harness.channel("a")
		path := reraceFlow(4, 1, 46003)
		harness.syn(path)
		harness.sleep(time.Second)
		harness.syn(path)
		if got := a.dialFailureCount(); got != 1 {
			t.Fatalf("A carries %d strikes after two SYNs of one attempt, want 1", got)
		}
		reconnect := *path
		reconnect.SequenceNumber += 1000
		harness.sleep(time.Second)
		harness.syn(&reconnect)
		if got := len(harness.exit("a").syns(path)); got != 3 {
			t.Fatalf("A received %d SYNs, want 3", got)
		}
		if got := a.dialFailureCount(); got != 2 {
			t.Errorf("A carries %d strikes after a second connect attempt on the 5-tuple, want 2", got)
		}
	})
}

// RR-6. A repeat still re-races. A flow whose only exit keeps failing it is
// left unbound after each failure, as after the first, so the moment an exit
// that can reach the destination joins, the next retransmit takes it. The
// repeat only adds no strike and no counted re-race.
func TestDialFailureRepeatOnTheOnlyExitStillReraces(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
		}, nil)
		a := harness.channel("a")
		path := reraceFlow(4, 1, 46002)
		harness.syn(path)
		harness.sleep(time.Second)
		// the retransmit goes back to A, the only exit, and fails again
		harness.syn(path)
		if got := len(harness.exit("a").syns(path)); got != 2 {
			t.Fatalf("A received %d SYNs, want both", got)
		}
		if got := a.dialFailureCount(); got != 1 {
			t.Errorf("A carries %d strikes for one flow, want 1", got)
		}
		if harness.boundTo(path) != nil {
			t.Error("the repeat left the flow bound to the exit that failed it")
		}

		// an exit that can reach the destination joins
		harness.addExit(reraceExitSpec{name: "c", tier: 0})
		harness.waitForExits(2)
		harness.syn(path)
		if _, ok := harness.synAck(path); !ok {
			t.Fatal("the retransmit after the exit joined did not complete")
		}
		if harness.boundTo(path) != harness.channel("c") {
			t.Error("the flow did not complete on the exit that joined")
		}
		if got := harness.metrics().FlowsReraced; got != 1 {
			t.Errorf("FlowsReraced = %d, want 1: the repeat is not counted again", got)
		}
	})
}

// --- RR-7: the v6 downgrade ---

// RR-7. A dual-stack exit whose v6 dials fail across two destinations while
// its v4 connects is downgraded to v4-only by the signals: each failed v6
// flow is re-raced with nothing reaching the app, the exit's monitor
// category changes in place, the window fills the v6 shortfall from another
// exit, new v6 flows go only to the v6-capable exit and v4 flows still use
// the downgraded one.
func TestDialFailureDowngradesStarvedIpv6AndFillsShortfall(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, family: IpFamilyDualstack, answer: func(path *IpPath) reraceAnswer {
				if path.Version == 6 {
					return reraceSignal
				}
				return reraceAccept
			}},
			{name: "b", tier: 1, family: IpFamilyV4Only},
		}, nil)
		a := harness.channel("a")
		window := harness.multiClient.windows[WindowTypeQuality]
		addedAt := window.monitor.ProviderEvents()[a.ClientId()].EventTime
		// a v6-only provider is drawn only by a v6-capable request, which the
		// window makes only while it holds no v6-capable exit
		c := harness.addExit(reraceExitSpec{name: "c", tier: 0, family: IpFamilyV6Only})

		v4 := reraceFlow(4, 0, 42000)
		if _, _, ok := harness.connect(v4); !ok || harness.boundTo(v4) != a {
			t.Fatal("A's v4 connect did not complete")
		}
		v6Flows := []*IpPath{reraceFlow(6, 1, 42001), reraceFlow(6, 2, 42002), reraceFlow(6, 1, 42003)}
		for i, path := range v6Flows {
			harness.syn(path)
			if got := len(harness.exit("a").syns(path)); got != 1 {
				t.Fatalf("v6 flow %d: A received %d SYNs, want 1", i, got)
			}
			if harness.boundTo(path) != nil {
				t.Errorf("v6 flow %d was not re-raced", i)
			}
			if got := len(harness.delivered(path)); got != 0 {
				t.Errorf("v6 flow %d: the app received %d packets, want none", i, got)
			}
			if i < 2 && a.IpFamily() != IpFamilyDualstack {
				t.Fatalf("A was downgraded after %d v6 strikes", i+1)
			}
		}

		if got := a.IpFamily(); got != IpFamilyV4Only {
			t.Fatalf("A's category = %q, want v4-only", got)
		}
		event := window.monitor.ProviderEvents()[a.ClientId()]
		if event == nil || event.IpFamily != IpFamilyV4Only || event.State != ProviderStateAdded || !event.EventTime.Equal(addedAt) {
			t.Errorf("A's monitor event = %+v, want v4-only republished in place", event)
		}
		downgrade := relEvent("family_downgrade", "exit", a.ClientId(), "to", string(IpFamilyV4Only), "dialfails", 3)
		if got := len(harness.log.linesWith(downgrade)); got != 1 {
			t.Errorf("%d %q events, want 1", got, downgrade)
		}

		// the shortfall is filled by the v6-only provider
		harness.waitForExits(3)
		filled := harness.channel("c")
		if filled.IpFamily() != IpFamilyV6Only || !harness.multiClient.Ipv6Available() {
			t.Errorf("the filled exit's category = %q and v6 available = %t, want v6-only and true", filled.IpFamily(), harness.multiClient.Ipv6Available())
		}

		// new v6 flows use only the v6-capable exit; v4 flows still use A
		for i := range 3 {
			path := reraceFlow(6, 3+i, 42010+i)
			if _, elapsed, ok := harness.connect(path); !ok || reraceSettle < elapsed {
				t.Errorf("v6 flow %d: completed=%t after %s, want at once", i, ok, elapsed)
			}
			if harness.boundTo(path) != filled || 0 < len(harness.exit("a").received(path)) {
				t.Errorf("v6 flow %d did not go to the v6-capable exit alone", i)
			}
		}
		next := reraceFlow(4, 6, 42020)
		if _, _, ok := harness.connect(next); !ok || harness.boundTo(next) != a {
			t.Error("a v4 flow no longer goes to the downgraded exit")
		}
		if got := len(c.received(next)); got != 0 {
			t.Errorf("the v6-only exit received %d packets of a v4 flow", got)
		}
	})
}

// RR-7. A v6-only exit is never downgraded: its v6 strikes across two
// destinations leave its category and its monitor event alone, and read as
// plain starvation instead.
func TestDialFailureNeverDowngradesIpv6OnlyExit(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		// the v6-only provider joins through the v6 shortfall that the v4-only
		// exit leaves open
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "v4", tier: 1, family: IpFamilyV4Only},
			{name: "v6", tier: 0, family: IpFamilyV6Only, answer: reraceAlways(reraceSignal)},
		}, nil)
		v6 := harness.channel("v6")
		window := harness.multiClient.windows[WindowTypeQuality]
		before := *window.monitor.ProviderEvents()[v6.ClientId()]
		if before.IpFamily != IpFamilyV6Only {
			t.Fatalf("setup: the v6-only exit's monitor category = %q", before.IpFamily)
		}

		for i, path := range []*IpPath{reraceFlow(6, 1, 43001), reraceFlow(6, 2, 43002), reraceFlow(6, 1, 43003)} {
			harness.syn(path)
			if got := len(harness.exit("v6").syns(path)); got != 1 {
				t.Fatalf("v6 flow %d: the v6-only exit received %d SYNs, want 1", i, got)
			}
			if harness.boundTo(path) != nil {
				t.Errorf("v6 flow %d was not re-raced", i)
			}
		}

		if got := v6.IpFamily(); got != IpFamilyV6Only {
			t.Errorf("the v6-only exit's category = %q, want v6-only", got)
		}
		after := window.monitor.ProviderEvents()[v6.ClientId()]
		if after == nil || after.IpFamily != before.IpFamily || after.State != before.State || !after.EventTime.Equal(before.EventTime) {
			t.Errorf("the v6-only exit's monitor event changed: %+v, want %+v", after, before)
		}
		if got := len(harness.log.linesWith(relEvent("family_downgrade"))); got != 0 {
			t.Errorf("%d family_downgrade events, want none", got)
		}
		if !v6.dialStarved() {
			t.Error("the v6-only exit's strikes did not read as starvation")
		}
	})
}

// RR-7, IPV6.md C7. A dual-stack exit whose IPv6 works keeps it when two
// hosts have a blackholed AAAA. The app sends each IPv6 SYN, connects over
// IPv4 250ms later and abandons the IPv6 attempt (happy eyeballs), so it never
// retransmits and the silence inference never fires. The provider's dials
// time out at its ConnectTimeout, and it answers each as classifyDialFailure
// decides: with nothing. A signal then could speed nothing up; it would only
// strike the exit for a flow no one waits on, and three strikes across the
// two hosts would make the exit v4-only for the life of the window and here
// take IPv6 out of the session.
func TestDialTimeoutOfAbandonedFlowsKeepsHealthyIpv6Exit(t *testing.T) {
	timeout := dialTimeoutError(t)
	connectTimeout := DefaultConnectSettings().ConnectTimeout
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, family: IpFamilyDualstack, answer: func(path *IpPath) reraceAnswer {
				if path.Version == 6 && path.DestinationIp[len(path.DestinationIp)-1] != 10 {
					// the provider's dial to the blackholed AAAA is pending
					return reraceSilent
				}
				return reraceAccept
			}},
			{name: "b", tier: 1, family: IpFamilyV4Only},
		}, nil)
		a := harness.channel("a")

		// A's IPv6 works
		healthy := reraceFlow(6, 0, 44000)
		if _, _, ok := harness.connect(healthy); !ok || harness.boundTo(healthy) != a {
			t.Fatal("setup: A's IPv6 connect did not complete")
		}
		harness.sleep(dialStrikeWindow + time.Second)

		losers := []*IpPath{reraceFlow(6, 1, 44001), reraceFlow(6, 2, 44002), reraceFlow(6, 1, 44003)}
		for i, loser := range losers {
			harness.syn(loser)
			harness.sleep(250 * time.Millisecond)
			winner := reraceFlow(4, 1+i%2, 44101+i)
			if _, _, ok := harness.connect(winner); !ok {
				t.Fatalf("setup: IPv4 connect %d did not complete", i)
			}
		}
		if got := harness.metrics().FlowsReraced; got != 0 {
			t.Fatalf("setup: %d re-races before the dials timed out", got)
		}

		// the provider's dials time out, and it answers each as it decides
		harness.sleep(connectTimeout)
		for _, loser := range losers {
			if _, signal := classifyDialFailure(reraceProviderFlowPath(loser), timeout, connectTimeout); signal != nil {
				harness.inject("a", signal)
			}
		}

		if got := a.IpFamily(); got != IpFamilyDualstack {
			t.Errorf("A's category = %q after dial timeouts of abandoned flows to two broken hosts, want dualstack: A's IPv6 works", got)
		}
		if !harness.multiClient.Ipv6Available() {
			t.Error("IPv6 is no longer available through the window")
		}
		if got := a.dialFailureCount(); got != 0 {
			t.Errorf("A carries %d dial strikes for flows the app had abandoned", got)
		}
	})
}

// --- RR-8: no storm ---

// RR-8. In the default wide race, where every exit of the best tier gets the
// SYN, a destination every exit fails costs at most one attempt per exit per
// app SYN: the signals of race candidates re-race nothing, and the app hears
// nothing before its own timeout. Exits that answer nothing but signals are
// benched for silence and replaced as the SYNs go on, so a later SYN can find
// a single exit; a race of one commits at once, and that exit's signal does
// re-race the flow. Only those SYNs may account for a re-race.
func TestDialFailureEveryExitFailsInWideRaceStaysBounded(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", answer: reraceAlways(reraceSignal)},
			{name: "b", answer: reraceAlways(reraceSignal)},
			{name: "c", answer: reraceAlways(reraceSignal)},
		}, nil)
		path := reraceFlow(4, 1, 45000)
		synCount, _, ok := harness.connect(path)
		if ok {
			t.Fatal("the connect completed although every exit fails")
		}
		exitSyns := 0
		sent := harness.sentSyns(path)
		// how many exits each app SYN reached: the last app SYN sent before
		// an exit's SYN is the one it forwards
		receivers := make([]int, len(sent))
		for _, name := range []string{"a", "b", "c"} {
			for _, packet := range harness.exit(name).syns(path) {
				exitSyns += 1
				j := len(sent) - 1
				for 0 <= j && packet.at.Before(sent[j]) {
					j -= 1
				}
				if 0 <= j {
					receivers[j] += 1
				}
			}
		}
		if exitSyns == 0 || 3*synCount < exitSyns {
			t.Errorf("the exits received %d SYNs for %d app SYNs, want at most one per exit each", exitSyns, synCount)
		}
		unraced := 0
		for _, count := range receivers {
			if count == 1 {
				unraced += 1
			}
		}
		metrics := harness.metrics()
		if uint64(unraced) < metrics.FlowsReraced {
			t.Errorf("FlowsReraced = %d, but only %d app SYNs reached a single exit (per SYN %v): a race candidate's signal unbinds nothing", metrics.FlowsReraced, unraced, receivers)
		}
		if metrics.DialFailuresIntercepted != uint64(exitSyns) {
			t.Errorf("DialFailuresIntercepted = %d, want one per failed dial (%d)", metrics.DialFailuresIntercepted, exitSyns)
		}
		if got := len(harness.delivered(path)); got != 0 {
			t.Errorf("the app received %d packets, want none before its own timeout", got)
		}
	})
}

// --- RR-9: metrics ---

// RR-9. DialFailuresIntercepted counts every intercepted signal, matched or
// not; FlowsReraced counts re-races only; a probe's signal and a packet that
// is not a signal count in neither. Both reset together.
func TestDialFailureMetricsCountEverySignalAndOnlyReraces(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: func(path *IpPath) reraceAnswer {
				if path.SourcePort == 46001 {
					return reraceSignal
				}
				return reraceAccept
			}},
			{name: "b", tier: 1},
		}, nil)
		reraced := reraceFlow(4, 1, 46001)
		established := reraceFlow(4, 2, 46002)
		if _, _, ok := harness.connect(reraced); !ok {
			t.Fatal("setup: the re-raced connect did not complete")
		}
		if _, _, ok := harness.connect(established); !ok {
			t.Fatal("setup: the established connect did not complete")
		}
		check := func(step string, intercepted uint64, reracedCount uint64) {
			t.Helper()
			metrics := harness.metrics()
			if metrics.DialFailuresIntercepted != intercepted || metrics.FlowsReraced != reracedCount {
				t.Errorf("%s: DialFailuresIntercepted=%d FlowsReraced=%d, want %d and %d",
					step, metrics.DialFailuresIntercepted, metrics.FlowsReraced, intercepted, reracedCount)
			}
		}
		check("matched", 1, 1)

		harness.inject("a", reraceProviderSignal(t, reraceFlow(4, 3, 46003)))
		check("no such flow", 2, 1)
		harness.inject("b", reraceProviderSignal(t, established))
		check("not the owner", 3, 1)
		harness.inject("a", reraceProviderSignal(t, established))
		check("established", 4, 1)
		harness.inject("a", reraceProviderSignal(t, reraced))
		check("after the re-race", 5, 1)
		probe := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        probeSourceIp4,
			SourcePort:      probeSourcePortMin,
			DestinationIp:   net.IPv4(93, 184, 216, 34),
			DestinationPort: 443,
		}
		harness.inject("a", reraceProviderSignal(t, probe))
		check("probe address", 5, 1)
		timeExceeded := reraceIcmpRetype(reraceProviderSignal(t, established), 11, 0)
		harness.inject("a", timeExceeded)
		check("not a signal", 5, 1)

		harness.multiClient.ResetReliabilityMetrics()
		check("reset", 0, 0)
	})
}

// RR-9. An intercepted signal is never received data: an exit that answers
// every dial with the signal has, to blackhole detection, received nothing --
// its stats convict it of no-receive-syn exactly like a silent exit's, where
// the exit that answered with SYN-ACKs is acquitted -- and the window removes
// it while keeping the other.
func TestDialFailureOnlyExitIsNotHealthyToBlackholeDetection(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
			{name: "b", tier: 2},
		}, nil)
		a, b := harness.channel("a"), harness.channel("b")
		for i := range 3 {
			if _, _, ok := harness.connect(reraceFlow(4, 1+i, 47000+i)); !ok {
				t.Fatalf("flow %d did not complete on B", i)
			}
		}
		if got := harness.metrics().DialFailuresIntercepted; got != 3 {
			t.Fatalf("DialFailuresIntercepted = %d, want 3", got)
		}
		aStats, err := a.WindowStats()
		if err != nil {
			t.Fatal(err)
		}
		if aStats.sendSynCount != 3 || aStats.receiveSynCount != 0 || aStats.receiveAckCount != 0 {
			t.Fatalf("A's window: sent %d SYNs, received %d SYNs and %d packets; want 3, 0 and 0",
				aStats.sendSynCount, aStats.receiveSynCount, aStats.receiveAckCount)
		}
		bStats, err := b.WindowStats()
		if err != nil {
			t.Fatal(err)
		}
		connectTimeout := harness.multiClient.settings.BlackholeConnectTimeout
		later := time.Now().Add(connectTimeout)
		if reason, _ := blackholeReasonFromStats(later, aStats, 0, 0, connectTimeout, blackholeGates{}); reason != blackholeNoReceiveSyn {
			t.Errorf("A's verdict = %v, want no-receive-syn", reason)
		}
		if reason, _ := blackholeReasonFromStats(later, bStats, 0, 0, connectTimeout, blackholeGates{}); reason != blackholeNone {
			t.Errorf("B's verdict = %v, want none", reason)
		}

		deadline := time.Now().Add(5 * time.Minute)
		for !a.IsDone() && time.Now().Before(deadline) {
			harness.sleep(time.Second)
		}
		if !a.IsDone() {
			t.Error("the exit that answers every dial with the signal was never removed")
		}
		if b.IsDone() {
			t.Error("the exit that answers with SYN-ACKs was removed")
		}
	})
}

// --- RR-10: spoofed, malformed and other icmp ---

// reraceIcmpCase is one packet an exit sends for a flow it owns, which must
// leave the flow alone.
type reraceIcmpCase struct {
	name   string
	packet func(t *testing.T, path *IpPath) []byte
	// intercepted is how much the packet adds to DialFailuresIntercepted: 1
	// for a signal that names no live flow, 0 for a packet the intercept does
	// not take, -1 when either is right
	intercepted int
	// data marks a packet that is legitimately received data (an echo reply)
	data bool
	// udp runs the case on a quic flow instead of a tcp one
	udp bool
}

// reraceAssertIcmpIgnored runs each case against its own client: a fresh flow
// bound to a silent exit A, which sends the case's packet for it. The flow
// must stay bound, A must take no strike, the packet must not count as
// received data and the app must receive nothing for the flow; then A's
// genuine signal for the same flow must unbind it, so the fixture can tell
// the difference.
func reraceAssertIcmpIgnored(t *testing.T, version int, cases []reraceIcmpCase) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				harness := newReraceHarness(t, []reraceExitSpec{
					{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
					{name: "b", tier: 1},
				}, nil)
				a := harness.channel("a")
				path := reraceFlow(version, 1, 48000)
				if c.udp {
					path = reraceUdpFlow(version, 1, 48000)
					harness.send(ipOosUdpPacket(path, []byte("quic initial")))
				} else {
					harness.syn(path)
				}
				if harness.boundTo(path) != a {
					t.Fatal("setup: the flow is not bound to A")
				}

				harness.inject("a", c.packet(t, path))

				if harness.boundTo(path) != a {
					t.Error("the flow was unbound")
				}
				if got := len(harness.delivered(path)); got != 0 {
					t.Errorf("the app received %d packets for the flow", got)
				}
				if got := a.dialFailureCount(); got != 0 {
					t.Errorf("A dialFailureCount = %d, want 0", got)
				}
				metrics := harness.metrics()
				if metrics.FlowsReraced != 0 {
					t.Errorf("FlowsReraced = %d, want 0", metrics.FlowsReraced)
				}
				if got := int(metrics.DialFailuresIntercepted); 0 <= c.intercepted && got != c.intercepted {
					t.Errorf("DialFailuresIntercepted = %d, want %d", got, c.intercepted)
				}
				if stats, err := a.WindowStats(); !c.data && (err != nil || stats.receiveAckCount != 0) {
					t.Error("the packet counted as received data")
				}

				harness.inject("a", reraceProviderSignal(t, path))
				if harness.boundTo(path) != nil {
					t.Error("positive control: A's genuine signal did not unbind the flow")
				}
			})
		})
	}
}

// reraceIcmpRetype rewrites the icmp type and code of a signal and fixes its
// checksums, so it differs from the provider's signal in exactly those.
func reraceIcmpRetype(packet []byte, icmpType byte, code byte) []byte {
	offset := dialFailureIcmpOffset(packet)
	packet[offset] = icmpType
	packet[offset+1] = code
	return resealDialFailureSignal(packet)
}

// RR-10. Unrelated signals -- naming another source port, destination,
// destination port or protocol, or the flow's reverse direction -- leave the
// flow alone. Each is still an intercepted signal.
func TestDialFailureInterceptIgnoresUnrelatedSignal(t *testing.T) {
	GetMessagePoolAggregateStats()
	other := func(change func(path *IpPath)) func(t *testing.T, path *IpPath) []byte {
		return func(t *testing.T, path *IpPath) []byte {
			unrelated := *path
			change(&unrelated)
			return reraceProviderSignal(t, &unrelated)
		}
	}
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			reraceAssertIcmpIgnored(t, version, []reraceIcmpCase{
				{name: "another source port", intercepted: 1, packet: other(func(path *IpPath) { path.SourcePort += 1000 })},
				{name: "another destination", intercepted: 1, packet: other(func(path *IpPath) {
					path.DestinationIp = slices.Clone(path.DestinationIp)
					path.DestinationIp[len(path.DestinationIp)-1] += 100
				})},
				{name: "another destination port", intercepted: 1, packet: other(func(path *IpPath) { path.DestinationPort = 80 })},
				{name: "udp", intercepted: 1, packet: other(func(path *IpPath) { path.Protocol = IpProtocolUdp })},
				{name: "the reverse direction", intercepted: 1, packet: func(t *testing.T, path *IpPath) []byte {
					return reraceProviderSignal(t, path.Reverse())
				}},
			})
		})
	}
}

// RR-10. A signal whose outer header does not run from the flow's destination
// back to its source is not the provider's signal for the flow: another host
// as the outer source or destination is spoofed.
func TestDialFailureInterceptIgnoresSignalWithForeignOuterAddresses(t *testing.T) {
	GetMessagePoolAggregateStats()
	rewrite := func(source bool) func(t *testing.T, path *IpPath) []byte {
		return func(t *testing.T, path *IpPath) []byte {
			packet := reraceProviderSignal(t, path)
			var address []byte
			switch packet[0] >> 4 {
			case 4:
				address = packet[16:20]
				if source {
					address = packet[12:16]
				}
			default:
				address = packet[24:40]
				if source {
					address = packet[8:24]
				}
			}
			address[len(address)-1] ^= 0x55
			return resealDialFailureSignal(packet)
		}
	}
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			reraceAssertIcmpIgnored(t, version, []reraceIcmpCase{
				{name: "outer source is not the destination", intercepted: -1, packet: rewrite(true)},
				{name: "outer destination is not the source", intercepted: -1, packet: rewrite(false)},
			})
		})
	}
}

// RR-10. Malformed packets are ignored: a bad outer or embedded header
// length, an unknown version, an embed that is not tcp or udp, and an embed
// of the other ip family than the icmp carrying it.
func TestDialFailureInterceptIgnoresMalformedSignal(t *testing.T) {
	GetMessagePoolAggregateStats()
	edit := func(change func(packet []byte, embed int)) func(t *testing.T, path *IpPath) []byte {
		return func(t *testing.T, path *IpPath) []byte {
			packet := reraceProviderSignal(t, path)
			change(packet, dialFailureIcmpOffset(packet)+icmpUnreachableHeaderSize)
			return packet
		}
	}
	t.Run("ipv4", func(t *testing.T) {
		reraceAssertIcmpIgnored(t, 4, []reraceIcmpCase{
			{name: "outer header length below 20", packet: edit(func(packet []byte, embed int) { packet[0] = 0x44 })},
			{name: "outer header length past the packet", packet: edit(func(packet []byte, embed int) { packet[0] = 0x4f })},
			{name: "outer version 5", packet: edit(func(packet []byte, embed int) { packet[0] = 0x55 })},
			{name: "embedded header length below 20", packet: edit(func(packet []byte, embed int) { packet[embed] = 0x44 })},
			{name: "embedded header length past the embed", packet: edit(func(packet []byte, embed int) { packet[embed] = 0x4f })},
			{name: "embedded protocol icmp", packet: edit(func(packet []byte, embed int) { packet[embed+9] = byte(ipProtocolNumberIcmp4) })},
		})
	})
	t.Run("ipv6", func(t *testing.T) {
		reraceAssertIcmpIgnored(t, 6, []reraceIcmpCase{
			{name: "outer next header not icmpv6", packet: edit(func(packet []byte, embed int) { packet[6] = byte(ipProtocolNumberUdp) })},
			{name: "outer payload length past the packet", packet: edit(func(packet []byte, embed int) {
				binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)))
			})},
			{name: "embedded version 4 in a v6 embed", packet: edit(func(packet []byte, embed int) { packet[embed] = 0x40 })},
			{name: "embedded next header icmpv6", packet: edit(func(packet []byte, embed int) { packet[embed+6] = byte(ipProtocolNumberIcmp6) })},
		})
	})
}

// RR-10. An embed carried by an icmp of the other ip family -- a v6 flow's
// embed in an icmpv4 unreachable, a v4 flow's in an icmpv6 one -- is
// malformed and is ignored.
func TestDialFailureInterceptIgnoresEmbedOfTheOtherFamily(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d flow", version), func(t *testing.T) {
			reraceAssertIcmpIgnored(t, version, []reraceIcmpCase{
				{name: "embed in the other family's icmp", intercepted: -1, packet: reraceCrossFamilySignal},
			})
		})
	}
}

// reraceCrossFamilySignal is the provider's signal for path with its embed
// carried by a destination-unreachable of the other ip family, between
// addresses of that family.
func reraceCrossFamilySignal(t *testing.T, path *IpPath) []byte {
	signal := reraceProviderSignal(t, path)
	embed := signal[dialFailureIcmpOffset(signal)+icmpUnreachableHeaderSize:]
	outer := &IpPath{Version: 4, SourceIp: net.IPv4(203, 0, 113, 99).To4(), DestinationIp: net.IPv4(10, 1, 0, 2).To4()}
	protocolNumber, icmpType, code := ipProtocolNumberIcmp4, byte(icmpv4TypeDestinationUnreachable), byte(icmpv4CodeHostUnreachable)
	if path.Version == 4 {
		outer = &IpPath{Version: 6, SourceIp: net.ParseIP("2001:db8::99"), DestinationIp: net.ParseIP("fd00:1::2")}
		protocolNumber, icmpType, code = ipProtocolNumberIcmp6, byte(icmpv6TypeDestinationUnreachable), byte(icmpv6CodeNoRoute)
	}
	packet, icmp := ipTransportPacket(outer, protocolNumber, icmpUnreachableHeaderSize+len(embed))
	icmp[0], icmp[1] = icmpType, code
	copy(icmp[icmpUnreachableHeaderSize:], embed)
	return resealDialFailureSignal(packet)
}

// RR-10. Truncated packets are ignored: cut inside the outer header, inside
// the icmp header, inside the embedded header, and one byte short of the 8
// transport bytes the embed must carry.
func TestDialFailureInterceptIgnoresTruncatedSignal(t *testing.T) {
	GetMessagePoolAggregateStats()
	cut := func(length func(signal []byte) int) func(t *testing.T, path *IpPath) []byte {
		return func(t *testing.T, path *IpPath) []byte {
			signal := reraceProviderSignal(t, path)
			return signal[:length(signal)]
		}
	}
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			embedHeader := Ipv4HeaderSizeWithoutExtensions
			if version == 6 {
				embedHeader = Ipv6HeaderSize
			}
			reraceAssertIcmpIgnored(t, version, []reraceIcmpCase{
				{name: "inside the outer header", packet: cut(func(signal []byte) int { return dialFailureIcmpOffset(signal) - 1 })},
				{name: "inside the icmp header", packet: cut(func(signal []byte) int { return dialFailureIcmpOffset(signal) + 4 })},
				{name: "inside the embedded header", packet: cut(func(signal []byte) int {
					return dialFailureIcmpOffset(signal) + icmpUnreachableHeaderSize + embedHeader - 1
				})},
				{name: "seven transport bytes", packet: cut(func(signal []byte) int {
					return dialFailureIcmpOffset(signal) + icmpUnreachableHeaderSize + embedHeader + 7
				})},
			})
		})
	}
}

// RR-10. Other icmp types naming the flow are ignored: echo reply, source
// quench, redirect, time exceeded, parameter problem, and v6 packet too big.
func TestDialFailureInterceptIgnoresOtherIcmpTypes(t *testing.T) {
	GetMessagePoolAggregateStats()
	retype := func(icmpType byte, code byte) func(t *testing.T, path *IpPath) []byte {
		return func(t *testing.T, path *IpPath) []byte {
			return reraceIcmpRetype(reraceProviderSignal(t, path), icmpType, code)
		}
	}
	t.Run("ipv4", func(t *testing.T) {
		reraceAssertIcmpIgnored(t, 4, []reraceIcmpCase{
			{name: "echo reply", data: true, packet: retype(0, 0)},
			{name: "source quench", packet: retype(4, 0)},
			{name: "redirect", packet: retype(5, 1)},
			{name: "time exceeded", packet: retype(11, 0)},
			{name: "parameter problem", packet: retype(12, 0)},
		})
	})
	t.Run("ipv6", func(t *testing.T) {
		reraceAssertIcmpIgnored(t, 6, []reraceIcmpCase{
			{name: "packet too big", packet: retype(2, 0)},
			{name: "time exceeded", packet: retype(3, 0)},
			{name: "parameter problem", packet: retype(4, 0)},
			{name: "echo reply", data: true, packet: retype(129, 0)},
		})
	})
}

// RR-10. A destination-unreachable naming the flow with any code but the one
// a provider's failed dial carries -- host unreachable (v4) or no route (v6)
// for tcp, port unreachable for udp -- is not the signal and is ignored.
func TestDialFailureInterceptIgnoresOtherIcmpCodes(t *testing.T) {
	GetMessagePoolAggregateStats()
	recode := func(code byte) func(t *testing.T, path *IpPath) []byte {
		return func(t *testing.T, path *IpPath) []byte {
			signal := reraceProviderSignal(t, path)
			return reraceIcmpRetype(signal, signal[dialFailureIcmpOffset(signal)], code)
		}
	}
	t.Run("ipv4", func(t *testing.T) {
		reraceAssertIcmpIgnored(t, 4, []reraceIcmpCase{
			{name: "net unreachable", intercepted: -1, packet: recode(0)},
			{name: "protocol unreachable", intercepted: -1, packet: recode(2)},
			{name: "port unreachable", intercepted: -1, packet: recode(3)},
			{name: "fragmentation needed", intercepted: -1, packet: recode(4)},
			{name: "administratively prohibited", intercepted: -1, packet: recode(13)},
		})
	})
	t.Run("ipv6", func(t *testing.T) {
		reraceAssertIcmpIgnored(t, 6, []reraceIcmpCase{
			{name: "administratively prohibited", intercepted: -1, packet: recode(1)},
			{name: "address unreachable", intercepted: -1, packet: recode(3)},
			{name: "port unreachable", intercepted: -1, packet: recode(4)},
			{name: "reject route", intercepted: -1, packet: recode(6)},
		})
	})
	// a quic flow's signal is port unreachable; the tcp signal's code is not it
	t.Run("ipv4 quic", func(t *testing.T) {
		reraceAssertIcmpIgnored(t, 4, []reraceIcmpCase{
			{name: "host unreachable", udp: true, intercepted: -1, packet: recode(1)},
			{name: "administratively prohibited", udp: true, intercepted: -1, packet: recode(13)},
		})
	})
	t.Run("ipv6 quic", func(t *testing.T) {
		reraceAssertIcmpIgnored(t, 6, []reraceIcmpCase{
			{name: "no route", udp: true, intercepted: -1, packet: recode(0)},
			{name: "administratively prohibited", udp: true, intercepted: -1, packet: recode(1)},
		})
	})
}

// --- RR-11: concurrency ---

// RR-11. Signals race sends, retransmits and window resizes: many flows
// connect at once while signals, duplicates and strays for live flows arrive
// from every exit, the window is resized throughout, and exits leave the
// window and are replaced. Under -race this must stay clean, every flow must
// complete, no signal may reach the app (a removed exit's flows get their
// teardown reset), and no flow may stay booked under an exit it is not bound
// to.
func TestDialFailureSignalsRaceSendsRetransmitsAndResizes(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		failOdd := func(path *IpPath) reraceAnswer {
			if path.SourcePort%2 == 1 {
				return reraceSignal
			}
			return reraceAccept
		}
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: failOdd},
			{name: "b", tier: 1, answer: failOdd},
			{name: "c", tier: 1},
		}, nil)
		const flowCount = 48
		paths := []*IpPath{}
		for i := range flowCount {
			paths = append(paths, reraceFlow(4+2*(i%2), i%5, 49000+i))
		}

		harness.concurrent = true
		ctx, cancel := context.WithCancel(harness.ctx)
		var noise sync.WaitGroup
		noise.Add(3)
		go func() {
			// duplicates and strays for live flows, from every exit
			defer noise.Done()
			random := rand.New(rand.NewSource(1))
			names := []string{"a", "b", "c"}
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
				path := paths[random.Intn(flowCount)]
				exit := harness.exit(names[random.Intn(len(names))])
				for _, channel := range harness.multiClient.windows[WindowTypeQuality].unorderedClients() {
					if channel.Destination() == exit.destination && !channel.IsDone() {
						exit.sendTo(channel.ClientId(), reraceProviderSignal(t, path))
					}
				}
			}
		}()
		go func() {
			// resize passes, and the readouts that walk the same state
			defer noise.Done()
			window := harness.multiClient.windows[WindowTypeQuality]
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(20 * time.Millisecond):
				}
				window.resizeMonitor.NotifyAll()
				harness.multiClient.Exits()
				harness.multiClient.ReliabilityMetrics()
			}
		}()
		go func() {
			// a failing exit leaves the window and the window replaces it;
			// the exit that answers everything stays, so every flow can finish
			defer noise.Done()
			names := []string{"a", "b"}
			for i := 0; ; i += 1 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(300 * time.Millisecond):
				}
				exit := harness.exit(names[i%len(names)])
				for _, channel := range harness.multiClient.windows[WindowTypeQuality].unorderedClients() {
					if channel.Destination() == exit.destination && !channel.IsDone() {
						harness.multiClient.DropExit(channel.ClientId())
					}
				}
			}
		}()

		results := make([]bool, flowCount)
		var flows sync.WaitGroup
		for i, path := range paths {
			flows.Add(1)
			go func() {
				defer flows.Done()
				_, _, results[i] = harness.connect(path)
			}()
		}
		flows.Wait()
		cancel()
		noise.Wait()
		harness.concurrent = false
		synctest.Wait()

		for i, path := range paths {
			if !results[i] {
				t.Errorf("flow %d did not complete", i)
			}
			for _, packet := range harness.delivered(path) {
				if parsed, err := ParseIpPath(packet.packet); err != nil || !(parsed.Syn && parsed.Ack) && !parsed.Rst {
					t.Errorf("flow %d: the app received %x, want only its SYN-ACK or a teardown reset", i, packet.packet)
				}
			}
			reraceAssertNoErrorDelivered(t, harness, path)
		}
		metrics := harness.metrics()
		if metrics.DialFailuresIntercepted < metrics.FlowsReraced {
			t.Errorf("FlowsReraced=%d exceeds DialFailuresIntercepted=%d", metrics.FlowsReraced, metrics.DialFailuresIntercepted)
		}
		multiClient := harness.multiClient
		multiClient.stateLock.Lock()
		defer multiClient.stateLock.Unlock()
		for client, updates := range multiClient.clientUpdates {
			for update := range updates {
				if update.client.Load() != client {
					t.Errorf("flow %+v is booked under an exit it is not bound to", update.ipPath)
				}
			}
		}
	})
}

// --- RR-12: compatibility ---

// RR-12. A provider that never sends the signal -- every provider before it
// did -- leaves the flow as before: an exit that carries live traffic but
// silently cannot reach one destination keeps that flow unanswered, nothing
// moves it before the 3s silence inference, and the app hears nothing until
// its own connect timeout.
func TestDialFailureSilentProviderLeavesFlowToTheAppTimeout(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", answer: func(path *IpPath) reraceAnswer {
				if path.DestinationIp[len(path.DestinationIp)-1] == 11 {
					return reraceSilent
				}
				return reraceAccept
			}},
		}, nil)
		a := harness.channel("a")
		live := reraceFlow(4, 0, 50000)
		if _, _, ok := harness.connect(live); !ok {
			t.Fatal("setup: the live flow did not connect")
		}

		harness.concurrent = true
		ctx, cancel := context.WithCancel(harness.ctx)
		var keepalive sync.WaitGroup
		keepalive.Add(1)
		go func() {
			// the exit's other traffic keeps flowing
			defer keepalive.Done()
			for offset := uint32(0); ; offset += 4 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				harness.send(reraceSegment(live, offset, []byte("ping")))
			}
		}()
		path := reraceFlow(4, 1, 50001)
		start := time.Now()
		var synCount int
		var completed bool
		connected := make(chan struct{})
		go func() {
			defer close(connected)
			synCount, _, completed = harness.connect(path)
		}()

		// past the first retransmit, before the silence inference
		time.Sleep(inferredDialFailureTimeout - reraceSettle)
		if harness.boundTo(path) != a {
			t.Error("the flow left its exit before the silence inference")
		}
		if metrics := harness.metrics(); metrics.DialFailuresIntercepted != 0 || metrics.FlowsReraced != 0 {
			t.Errorf("DialFailuresIntercepted=%d FlowsReraced=%d before the inference, want 0 and 0", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
		}

		<-connected
		cancel()
		keepalive.Wait()
		harness.concurrent = false
		if completed {
			t.Fatal("a connect to a destination the exit cannot reach completed")
		}
		if synCount != len(reraceSynSchedule) {
			t.Errorf("the app sent %d SYNs, want its whole schedule", synCount)
		}
		if elapsed := time.Since(start); elapsed < reraceSynTimeout {
			t.Errorf("the connect ended after %s, before the app's own timeout", elapsed)
		}
		if got := len(harness.delivered(path)); got != 0 {
			t.Errorf("the app received %d packets for the flow, want none", got)
		}
		if a.IsDone() {
			t.Error("the exit was removed")
		}
	})
}

// RR-12. Without the signal, a flow on a silent exit recovers through the
// silence inference at the retransmit 3s after its first SYN, as before the
// signal existed; the signal is what moves it at 1s (RR-1).
func TestDialFailureSilentProviderRecoversAtTheInference(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1},
		}, nil)
		path := reraceFlow(4, 1, 50002)
		synCount, elapsed, ok := harness.connect(path)
		if !ok {
			t.Fatal("the connect never completed")
		}
		if synCount != 3 || elapsed < reraceSynSchedule[2] || reraceSynSchedule[2]+reraceSettle < elapsed {
			t.Errorf("the connect completed on SYN %d after %s, want on SYN 3 at %s", synCount, elapsed, reraceSynSchedule[2])
		}
		if harness.boundTo(path) != harness.channel("b") {
			t.Error("the flow is not on B")
		}
		if metrics := harness.metrics(); metrics.FlowsReraced != 1 {
			t.Errorf("FlowsReraced = %d, want the one inferred re-race", metrics.FlowsReraced)
		}
	})
}

// RR-12. The silence inference is not uplink ingress: it fires on the send
// path because nothing came back, so it neither stamps the uplink as
// delivering nor counts as an intercepted signal. While apps retransmit into
// a dead local uplink, a stamp would hold the uplink staleness gate open, and
// silence verdicts against exits stay admissible through the very outage the
// gate exists for.
func TestDialFailureInferenceIsNotUplinkIngress(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: func(path *IpPath) reraceAnswer {
				if path.DestinationIp[len(path.DestinationIp)-1] == 11 {
					return reraceSilent
				}
				return reraceAccept
			}},
			{name: "b", tier: 1, answer: reraceAlways(reraceSilent)},
		}, nil)
		live := reraceFlow(4, 0, 45000)
		if _, _, ok := harness.connect(live); !ok {
			t.Fatal("setup: the live flow did not connect")
		}
		lastIngress := harness.multiClient.uplinkLastIngressNanos.Load()
		if lastIngress == 0 {
			t.Fatal("setup: no ingress was stamped")
		}

		// from here on nothing arrives from any exit
		harness.sleep(10 * time.Second)
		path := reraceFlow(4, 1, 45001)
		harness.syn(path)
		harness.sleep(time.Second)
		harness.syn(path)
		harness.sleep(2 * time.Second)
		// the 3s retransmit trips the silence inference
		harness.syn(path)
		if got := harness.metrics().FlowsReraced; got != 1 {
			t.Fatalf("setup: FlowsReraced = %d, want the inferred re-race", got)
		}

		if got := harness.multiClient.uplinkLastIngressNanos.Load(); got != lastIngress {
			t.Errorf("the silence inference stamped uplink ingress %s after the last packet arrived", time.Duration(got-lastIngress))
		}
		if got := harness.metrics().DialFailuresIntercepted; got != 0 {
			t.Errorf("DialFailuresIntercepted = %d, but no signal was intercepted", got)
		}
	})
}

// RR-12. A client from before the intercept drops the signal harmlessly:
// ParseIpPath rejects every signal a provider sends, tcp and udp of both
// families, and a channel with no intercept receives them without delivering
// anything, counting them, or failing.
func TestOldClientDropsDialFailureSignalHarmlessly(t *testing.T) {
	signals := [][]byte{}
	for _, version := range []int{4, 6} {
		signals = append(signals,
			reraceProviderSignal(t, reraceFlow(version, 1, 51000)),
			reraceProviderSignal(t, reraceUdpFlow(version, 1, 51001)),
		)
	}
	for _, signal := range signals {
		if _, err := ParseIpPath(signal); err == nil {
			t.Errorf("ParseIpPath accepted the signal %x", signal)
		}
		if _, _, err := ParseIpPathWithPayload(signal); err == nil {
			t.Errorf("ParseIpPathWithPayload accepted the signal %x", signal)
		}
	}

	delivered := 0
	channel := &multiClientChannel{
		ctx:         context.Background(),
		log:         NewNoopLogger(),
		args:        &multiClientChannelArgs{},
		settings:    DefaultMultiClientSettings(),
		packetStats: &clientWindowStats{log: NewNoopLogger()},
		clientReceivePacketCallback: func(*multiClientChannel, TransferPath, protocol.ProvideMode, TransportType, *IpPath, []byte) {
			delivered += 1
		},
	}
	frames := []*protocol.Frame{}
	for _, signal := range signals {
		frames = append(frames, RequireToFrameWithDefaultProtocolVersion(&protocol.IpPacketFromProvider{
			IpPacket: &protocol.IpPacket{PacketBytes: signal},
		}))
	}
	channel.clientReceive(TransferPath{}, frames, Peer{ProvideMode: protocol.ProvideMode_Network})
	if delivered != 0 {
		t.Errorf("%d signals were delivered as packets", delivered)
	}
	if channel.packetStats.receiveAckCount != 0 || channel.packetStats.receiveSynCount != 0 {
		t.Errorf("the signals were counted as received data: %d packets, %d SYNs", channel.packetStats.receiveAckCount, channel.packetStats.receiveSynCount)
	}
}
