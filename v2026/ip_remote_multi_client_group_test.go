// Homogeneous packet-group routing, policy, and ownership regressions.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Records payload-aware policy calls and can reject a later group member.
type groupTestSecurityPolicy struct {
	stats        *SecurityPolicyStatsCollector
	inspectCount atomic.Int32
	refreshCount atomic.Int32
}

// Returns the collector used by the ordinary client diagnostics.
func (self *groupTestSecurityPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

// Blocks the member carrying the test's explicit marker.
func (self *groupTestSecurityPolicy) InspectEgress(
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	payload []byte,
) (SecurityPolicyResult, error) {
	self.inspectCount.Add(1)
	if len(payload) != 0 && payload[0] == 0xff {
		return SecurityPolicyResultDrop, nil
	}
	return SecurityPolicyResultAllow, nil
}

// Allows the unused return direction.
func (self *groupTestSecurityPolicy) InspectIngress(
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	payload []byte,
) (SecurityPolicyResult, error) {
	return SecurityPolicyResultAllow, nil
}

// Counts activity refreshes beside the inspections.
func (self *groupTestSecurityPolicy) RefreshEgress(ipPath *IpPath) {
	self.refreshCount.Add(1)
}

// The return direction is unused by these send-path tests.
func (self *groupTestSecurityPolicy) RefreshIngress(ipPath *IpPath) {
}

// Builds a bare parent whose real routing path is replaced by deterministic
// selected-client seams. The returned close function owns the update context.
func groupTestParent(
	t *testing.T,
	policy SecurityPolicy,
) (*RemoteUserNatMultiClient, *multiClientChannelUpdate, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultMultiClientSettings()
	settings.TcpCollapsePrevention = false
	parent := &RemoteUserNatMultiClient{
		ctx:                  ctx,
		cancel:               cancel,
		generator:            &testingEmptyMultiClientGenerator{},
		settings:             settings,
		provideMode:          protocol.ProvideMode_Network,
		log:                  NewNoopLogger(),
		securityPolicy:       policy,
		localUserNat:         NewLocalUserNat(ctx, "group test", DefaultLocalUserNatSettings()),
		blockActionCache:     newBlockActionCache(time.Minute, 16),
		blockActionCollector: newBlockActionCollector(16, NewNoopLogger()),
		packetStatsCounters:  &packetStatsCounters{},
		reliabilityMetrics:   newReliabilityMetrics(),
		windows:              map[WindowType]*multiClientWindow{},
		clientUpdates:        map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}
	parent.config.Store(&multiClientConfig{})
	parent.blockActionState.Store(&blockActionState{})
	parent.blockActionIgnoreState.Store(&blockActionIgnoreState{})
	update := newMultiClientChannelUpdate(ctx, nil)
	parent.sendClientPathForTest = func(
		ipPath *IpPath,
		pin flowPin,
		callback func(*multiClientChannelUpdate, *multiClientChannel),
	) {
		update.ipPath = ipPath
		callback(update, update.client.Load())
	}
	return parent, update, func() {
		update.Close()
		parent.localUserNat.Close()
		cancel()
	}
}

// Retains one reference per packet so a successful sender can prove it
// consumed exactly the caller-owned reference without racing pool reuse.
func groupTestPacketWitnesses(t *testing.T, packets [][]byte) [][]byte {
	t.Helper()
	witnesses := make([][]byte, len(packets))
	for packetIndex, packet := range packets {
		if pooled, _ := MessagePoolCheck(packet); !pooled {
			for _, witness := range witnesses[:packetIndex] {
				MessagePoolReturn(witness)
			}
			t.Fatalf("packet %d is not pooled", packetIndex)
		}
		witnesses[packetIndex] = MessagePoolShareReadOnly(packet)
	}
	return witnesses
}

// Proves every production owner was returned before releasing the retained
// witnesses. On failure it also releases the leaked owner to isolate tests.
func requireGroupTestWitnessesReleased(t *testing.T, packets [][]byte, witnesses [][]byte) {
	t.Helper()
	for packetIndex, witness := range witnesses {
		if MessagePoolReturn(witness) {
			continue
		}
		MessagePoolReturn(packets[packetIndex])
		t.Errorf("packet %d retained an owner after successful group send", packetIndex)
	}
}

func groupTestPoolOutstanding() int64 {
	taken, returned, _ := MessagePoolCounts()
	return int64(taken) - int64(returned)
}

// Builds the minimal real channel state needed by the stalled-send ownership
// paths. No client is required because a stalled channel never enters Transfer.
func groupTestStalledChannel(protocolVersion int) *multiClientChannel {
	settings := DefaultMultiClientSettings()
	settings.ProtocolVersion = protocolVersion
	client := &multiClientChannel{
		ctx:                       context.Background(),
		log:                       NewNoopLogger(),
		settings:                  settings,
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{log: NewNoopLogger()},
	}
	client.stalled.Store(true)
	return client
}

// Converts ordinary packets into the owned-header group used by the send path.
func requireGroupTestPacketGroup(t *testing.T, packets ...[]byte) *ipPacketGroup {
	t.Helper()
	groups, rejectedPackets := groupIpPackets(packets)
	if len(rejectedPackets) != 0 || len(groups) != 1 {
		t.Fatalf("grouped %d groups with %d rejects, want one group and no rejects", len(groups), len(rejectedPackets))
	}
	return groups[0]
}

// A later payload verdict applies to the whole group before any provider is
// selected. The old singular send could transmit the first member first.
func TestMultiClientPacketGroupLaterPolicyBlockRejectsWholeGroup(t *testing.T) {
	policy := &groupTestSecurityPolicy{stats: DefaultSecurityPolicyStatsCollector()}
	parent, _, closeParent := groupTestParent(t, policy)
	defer closeParent()

	selectionCount := atomic.Int32{}
	parent.sendClientPathForTest = func(
		ipPath *IpPath,
		pin flowPin,
		callback func(*multiClientChannelUpdate, *multiClientChannel),
	) {
		selectionCount.Add(1)
		t.Fatal("blocked group reached provider selection")
	}

	packet1 := ipOosUdpPacket(udpTestPath(4), []byte{1})
	packet2 := ipOosUdpPacket(udpTestPath(4), []byte{0xff})
	group := requireGroupTestPacketGroup(t, packet1, packet2)
	if parent.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, group, 0) {
		t.Fatal("group with a blocked later member was accepted")
	}
	if got := policy.inspectCount.Load(); got != 2 {
		t.Errorf("policy inspections = %d, want 2 ordered member scans", got)
	}
	if got := policy.refreshCount.Load(); got != 1 {
		t.Errorf("policy refreshes = %d, want one group activity refresh", got)
	}
	if got := selectionCount.Load(); got != 0 {
		t.Errorf("provider selections = %d, want 0", got)
	}
	stats := parent.PacketStats()
	if stats.BlockEgressPacketCount != 2 {
		t.Errorf("blocked packet count = %d, want 2", stats.BlockEgressPacketCount)
	}
}

// One selected-client call owns the complete group in source order.
func TestMultiClientPacketGroupSelectedClientAdmitsWholeGroupOnce(t *testing.T) {
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()

	var callCount atomic.Int32
	var finalAcceptedOwnerReturns atomic.Int32
	var receivedPayloads [][]byte
	client := &multiClientChannel{
		ctx:      parent.ctx,
		settings: parent.settings,
		sendGroupForTest: func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
			callCount.Add(1)
			for packetIndex := range group.packets {
				receivedPayloads = append(receivedPayloads, append([]byte(nil), group.packets[packetIndex].payload...))
				if MessagePoolReturn(group.packets[packetIndex].packet) {
					finalAcceptedOwnerReturns.Add(1)
				}
			}
			return true, nil
		},
	}
	update.client.Store(client)

	packet1 := MessagePoolCopy(ipOosUdpPacket(udpTestPath(4), []byte{1}))
	packet2 := MessagePoolCopy(ipOosUdpPacket(udpTestPath(4), []byte{2}))
	packet3 := MessagePoolCopy(ipOosUdpPacket(udpTestPath(4), []byte{3}))
	packets := [][]byte{packet1, packet2, packet3}
	witnesses := groupTestPacketWitnesses(t, packets)
	group := requireGroupTestPacketGroup(t, packet1, packet2, packet3)
	if !parent.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, group, 0) {
		t.Fatal("selected client rejected group")
	}
	if got := callCount.Load(); got != 1 {
		t.Fatalf("selected-client calls = %d, want 1", got)
	}
	if got := finalAcceptedOwnerReturns.Load(); got != 0 {
		t.Errorf("accepted owner final returns = %d, want 0 while witnesses are retained", got)
	}
	for packetIndex, payload := range receivedPayloads {
		if len(payload) != 1 || payload[0] != byte(packetIndex+1) {
			t.Errorf("payload %d = %v, want [%d]", packetIndex, payload, packetIndex+1)
		}
	}
	requireGroupTestWitnessesReleased(t, packets, witnesses)
}

// A one-candidate first send has no selected-client success commit. Its
// ordered SYN/RST controls must therefore reset stale collapse state before
// candidate admission, as the singular path did.
func TestMultiClientPacketGroupOneCandidateResetsControlSequenceBeforeSend(t *testing.T) {
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()

	update.ackSequenceNumber = 91
	update.sequenceNumber = 92
	update.sequencePacketCount = 93
	update.sequenceTime = time.Time{}

	tcpPath := udpTestPath(4)
	tcpPath.Protocol = IpProtocolTcp
	const synSequence = uint32(101)
	const rstSequence = uint32(202)
	packets := [][]byte{
		MessagePoolCopy(ipOosTcpPacketSequence(tcpPath, tcpFlagSyn, synSequence, nil)),
		MessagePoolCopy(ipOosTcpPacketSequence(tcpPath, tcpFlagRst, rstSequence, nil)),
	}
	witnesses := groupTestPacketWitnesses(t, packets)
	group := requireGroupTestPacketGroup(t, packets...)

	type sequenceState struct {
		ackSequenceNumber uint32
		sequenceNumber    uint32
		packetCount       int
		sequenceTime      time.Time
	}
	observedState := sequenceState{}
	var callCount atomic.Int32
	client := &multiClientChannel{
		ctx:      parent.ctx,
		settings: parent.settings,
		sendGroupForTest: func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
			callCount.Add(1)
			update.stateLock.Lock()
			observedState = sequenceState{
				ackSequenceNumber: update.ackSequenceNumber,
				sequenceNumber:    update.sequenceNumber,
				packetCount:       update.sequencePacketCount,
				sequenceTime:      update.sequenceTime,
			}
			update.stateLock.Unlock()
			for packetIndex := range group.packets {
				MessagePoolReturn(group.packets[packetIndex].packet)
			}
			return true, nil
		},
	}
	parent.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
		return []*multiClientChannel{client}
	}

	if !parent.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, group, 0) {
		t.Error("one-candidate control group was rejected")
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("candidate sends = %d, want 1", got)
	}
	if observedState.ackSequenceNumber != 0 ||
		observedState.sequenceNumber != rstSequence ||
		observedState.packetCount != 0 ||
		observedState.sequenceTime.IsZero() {
		t.Errorf(
			"pre-send sequence = ack:%d sequence:%d packets:%d time-zero:%t; want ack:0 sequence:%d packets:0 time-zero:false",
			observedState.ackSequenceNumber,
			observedState.sequenceNumber,
			observedState.packetCount,
			observedState.sequenceTime.IsZero(),
			rstSequence,
		)
	}
	if update.client.Load() != client {
		t.Error("one accepted candidate was not bound")
	}
	requireGroupTestWitnessesReleased(t, packets, witnesses)
}

// Every candidate gets every member as a read-only share, and the originals
// transfer exactly once only after all barrier-held attempts finish.
func TestMultiClientPacketGroupRaceKeepsMembersTogether(t *testing.T) {
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()

	type attempt struct {
		client  *multiClientChannel
		packets [][]byte
	}
	attempts := make(chan attempt, 2)
	finishAttempts := make(chan struct{})
	var finishOnce sync.Once
	finish := func() {
		finishOnce.Do(func() {
			close(finishAttempts)
		})
	}
	defer finish()
	var finalAcceptedOwnerReturns atomic.Int32
	makeClient := func() *multiClientChannel {
		client := &multiClientChannel{
			ctx:      parent.ctx,
			settings: parent.settings,
		}
		client.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
			packets := make([][]byte, len(group.packets))
			for packetIndex := range group.packets {
				packets[packetIndex] = group.packets[packetIndex].packet
			}
			attempts <- attempt{client: client, packets: packets}
			<-finishAttempts
			for _, packet := range packets {
				if MessagePoolReturn(packet) {
					finalAcceptedOwnerReturns.Add(1)
				}
			}
			return true, nil
		}
		return client
	}
	client1 := makeClient()
	client2 := makeClient()
	parent.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
		return []*multiClientChannel{client1, client2}
	}

	packet1 := MessagePoolCopy(ipOosUdpPacket(udpTestPath(4), []byte{1}))
	packet2 := MessagePoolCopy(ipOosUdpPacket(udpTestPath(4), []byte{2}))
	packets := [][]byte{packet1, packet2}
	witnesses := groupTestPacketWitnesses(t, packets)
	group := requireGroupTestPacketGroup(t, packet1, packet2)
	result := make(chan bool, 1)
	go func() {
		result <- parent.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, group, time.Second)
	}()

	attempt1 := <-attempts
	attempt2 := <-attempts
	for attemptIndex, observed := range []attempt{attempt1, attempt2} {
		if len(observed.packets) != 2 {
			t.Errorf("attempt %d packets = %d, want 2", attemptIndex, len(observed.packets))
		}
		for packetIndex, packet := range observed.packets {
			pooled, shared := MessagePoolCheck(packet)
			if !pooled || !shared {
				t.Errorf("attempt %d packet %d pooled/shared = %t/%t, want true/true", attemptIndex, packetIndex, pooled, shared)
			}
		}
	}
	finish()
	if !<-result {
		t.Error("whole-group race reported no accepted candidate")
	}
	if update.client.Load() != nil {
		t.Error("send-side group race committed before response evidence")
	}
	if attempt1.client == attempt2.client {
		t.Error("both attempts used the same candidate")
	}
	if got := finalAcceptedOwnerReturns.Load(); got != 0 {
		t.Errorf("accepted race-share final returns = %d, want 0 while original witness is retained", got)
	}
	requireGroupTestWitnessesReleased(t, packets, witnesses)
}

// A simulated stalled exit consumes the packet just like an admitted Transfer
// while retaining its outstanding health accounting. Raw and legacy framing
// must both release every accepted buffer owner.
func TestMultiClientPacketStalledSendReturnsAcceptedOwner(t *testing.T) {
	for _, protocolVersion := range []int{DefaultProtocolVersion, 1} {
		t.Run(protocolVersionName(protocolVersion), func(t *testing.T) {
			before := groupTestPoolOutstanding()
			packet := MessagePoolCopy(ipOosUdpPacket(udpTestPath(4), []byte{1}))
			packets := [][]byte{packet}
			witnesses := groupTestPacketWitnesses(t, packets)
			ipPath, payload, err := ParseIpPathWithPayload(packet)
			if err != nil {
				t.Fatalf("parse packet: %v", err)
			}
			client := groupTestStalledChannel(protocolVersion)
			success, err := client.SendDetailedWithAck(&parsedPacket{
				packet:  packet,
				ipPath:  ipPath,
				payload: payload,
			}, 0, false)
			if err != nil || !success {
				t.Fatalf("stalled singleton send = %t, %v; want true, nil", success, err)
			}
			requireGroupTestWitnessesReleased(t, packets, witnesses)
			if after := groupTestPoolOutstanding(); after != before {
				t.Errorf("stalled singleton pool outstanding = %d -> %d", before, after)
			}
		})
	}
}

// The group stalled path has the singleton contract for every member and must
// also release every legacy wrapper built before the simulated blackhole.
func TestMultiClientPacketGroupStalledSendReturnsAcceptedOwners(t *testing.T) {
	for _, protocolVersion := range []int{DefaultProtocolVersion, 1} {
		t.Run(protocolVersionName(protocolVersion), func(t *testing.T) {
			before := groupTestPoolOutstanding()
			packets := [][]byte{
				MessagePoolCopy(ipOosUdpPacket(udpTestPath(4), []byte{1})),
				MessagePoolCopy(ipOosUdpPacket(udpTestPath(4), []byte{2})),
				MessagePoolCopy(ipOosUdpPacket(udpTestPath(4), []byte{3})),
			}
			witnesses := groupTestPacketWitnesses(t, packets)
			group := requireGroupTestPacketGroup(t, packets...)
			parsedPackets := make([]parsedPacket, len(packets))
			for packetIndex, packet := range packets {
				_, payload, err := ParseIpPathWithPayload(packet)
				if err != nil {
					t.Fatalf("parse group packet %d: %v", packetIndex, err)
				}
				parsedPackets[packetIndex] = parsedPacket{
					packet:  packet,
					ipPath:  group.ipPath,
					payload: payload,
				}
			}
			client := groupTestStalledChannel(protocolVersion)
			success, err := client.SendGroupDetailedWithAck(&parsedPacketGroup{
				packets:   parsedPackets,
				ipPath:    group.ipPath,
				byteCount: group.byteCount,
			}, 0, false)
			if err != nil || !success {
				t.Fatalf("stalled group send = %t, %v; want true, nil", success, err)
			}
			requireGroupTestWitnessesReleased(t, packets, witnesses)
			if after := groupTestPoolOutstanding(); after != before {
				t.Errorf("stalled group pool outstanding = %d -> %d", before, after)
			}
		})
	}
}

func protocolVersionName(protocolVersion int) string {
	if 2 <= protocolVersion {
		return "raw"
	}
	return "legacy"
}
