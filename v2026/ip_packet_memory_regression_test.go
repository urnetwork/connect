package connect

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

func packetGroupAllocationFixture() (*[]*ipPacketGroup, map[ipPacketFlowKey]*ipPacketGroup, *IpPath, []byte, *ipPacketGroup) {
	packet := testingUdp4Packet("10.0.0.1", "203.0.113.7", 443, []byte("payload"))
	ipPath, _, _ := ParseIpPathWithPayload(packet)
	key, owned, _ := ownIpPacketFlow(ipPath)
	group := &ipPacketGroup{ipPath: owned, packets: make([][]byte, 0, 4), ipPaths: make([]IpPath, 0, 4), payloads: make([][]byte, 0, 4)}
	groups := []*ipPacketGroup{group}
	return &groups, map[ipPacketFlowKey]*ipPacketGroup{key: group}, ipPath, packet, group
}

func TestExistingPacketGroupDoesNotAllocateAnotherOwnedPath(t *testing.T) {
	groups, byKey, path, packet, group := packetGroupAllocationFixture()
	owned := group.ipPath
	allocs := testing.AllocsPerRun(1000, func() {
		group.packets, group.ipPaths, group.payloads = group.packets[:0], group.ipPaths[:0], group.payloads[:0]
		group.byteCount = 0
		if !appendIpPacketGroupBounded(groups, byKey, path, packet[28:], packet, 4, 4096) {
			panic("append failed")
		}
	})
	if allocs != 0 {
		t.Fatalf("existing group allocated %.0f objects, want 0", allocs)
	}
	if group.ipPath != owned || len(group.packets) != 1 || !bytes.Equal(group.packets[0], packet) {
		t.Fatal("group reuse changed packet/path identity")
	}
}

func committedBatchAllocationFixture() (*RemoteUserNatMultiClient, *multiClientChannel, []*IpPath, [][]byte) {
	packet := testingUdp4Packet("203.0.113.7", "10.0.0.1", 443, []byte("payload"))
	inbound, _, _ := ParseIpPathWithPayload(packet)
	outbound := inbound.ReverseValue()
	update := newMultiClientChannelUpdate(context.Background(), &outbound)
	client := &multiClientChannel{}
	update.client.Store(client)
	update.receivedInbound.Store(true)
	parent := &RemoteUserNatMultiClient{ctx: context.Background(), log: NewNoopLogger(), settings: DefaultMultiClientSettings(),
		securityPolicy: DisableSecurityPolicy(), packetStatsCounters: &packetStatsCounters{},
		ip4PathUpdates: map[Ip4Path]*multiClientChannelUpdate{outbound.ToIp4Path(): update}}
	parent.SetReceivePacketsCallback(func(TransferPath, protocol.ProvideMode, *IpPath, [][]byte) {})
	return parent, client, []*IpPath{inbound}, [][]byte{packet}
}

func TestCommittedBatchLookupDoesNotAllocateReversedPath(t *testing.T) {
	parent, client, paths, packets := committedBatchAllocationFixture()
	deliveries := 0
	parent.SetReceivePacketsCallback(func(_ TransferPath, _ protocol.ProvideMode, path *IpPath, got [][]byte) {
		if path != nil || len(got) != 1 || !bytes.Equal(got[0], packets[0]) {
			panic("batch changed")
		}
		deliveries++
	})
	allocs := testing.AllocsPerRun(1000, func() {
		parent.clientReceivePackets(client, TransferPath{}, protocol.ProvideMode_Network, TransportTypeUnknown, paths, packets)
	})
	// One packet-slice backing allocation belongs to batch delivery. Looking up
	// an already committed flow must not add one reversed IpPath per packet.
	if allocs != 1 {
		t.Fatalf("committed batch allocated %.0f objects, want only 1 delivery slice", allocs)
	}
	if deliveries != 1001 {
		t.Fatalf("deliveries = %d", deliveries)
	}
}

func rawReceiveAllocationFixture() (*multiClientChannel, []*protocol.Frame, *int) {
	client := probeTestChannel(DefaultMultiClientSettings())
	client.log = NewNoopLogger()
	packet := testingUdp4Packet("203.0.113.7", "10.0.0.1", 443, []byte("payload"))
	frames := []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}}
	delivered := new(int)
	client.args.ReceivePackets = func(_ *multiClientChannel, _ TransferPath, _ protocol.ProvideMode, _ TransportType, paths []*IpPath, packets [][]byte) {
		if len(paths) != 1 || len(packets) != 1 || !bytes.Equal(packets[0], packet) || paths[0].Protocol != IpProtocolUdp {
			panic("raw receive changed")
		}
		*delivered++
	}
	return client, frames, delivered
}

func TestRawChannelReceiveDoesNotAllocateLegacyProtoWrappers(t *testing.T) {
	client, frames, delivered := rawReceiveAllocationFixture()
	allocs := testing.AllocsPerRun(1000, func() { client.clientReceive(TransferPath{}, frames, Peer{}) })
	// Owned IP path + address backing, and the two delivery slices. No
	// IpPacketFromProvider/IpPacket wrapper belongs on the raw v2+ path.
	if allocs != 4 {
		t.Fatalf("raw receive allocated %.0f objects, want 4 without legacy wrappers", allocs)
	}
	if *delivered != 1001 {
		t.Fatalf("delivered = %d", *delivered)
	}
}

func TestRawReceiveOptimizationPreservesLegacyAndMalformedFrames(t *testing.T) {
	client, frames, delivered := rawReceiveAllocationFixture()
	legacy, err := ToFrame(&protocol.IpPacketFromProvider{IpPacket: &protocol.IpPacket{PacketBytes: frames[0].MessageBytes}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Raw {
		t.Fatal("fixture did not exercise legacy protobuf")
	}
	client.clientReceive(TransferPath{}, frames, Peer{})
	client.clientReceive(TransferPath{}, []*protocol.Frame{legacy}, Peer{})
	client.clientReceive(TransferPath{}, []*protocol.Frame{
		{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: false, MessageBytes: []byte{255}},
		{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: []byte{0x45}},
	}, Peer{})
	if *delivered != 2 {
		t.Fatalf("raw/legacy/malformed delivery count = %d, want 2", *delivered)
	}
	if client.packetStats.receiveAckCount != 2 {
		t.Fatalf("malformed input counted as receive evidence: %d", client.packetStats.receiveAckCount)
	}
}

func TestCommittedBatchLookupPreservesUnknownFlowFallbackAndInputPath(t *testing.T) {
	parent, client, paths, packets := committedBatchAllocationFixture()
	input := *paths[0]
	var returned *IpPath
	parent.SetReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, path *IpPath, packet []byte) { returned = path })
	parent.clientReceivePackets(client, TransferPath{}, protocol.ProvideMode_Network, TransportTypeUnknown, paths, packets)
	if returned != nil {
		t.Fatal("committed packet escaped the batch callback")
	}
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
	parent.clientReceivePackets(client, TransferPath{}, protocol.ProvideMode_Network, TransportTypeUnknown, paths, packets)
	if returned == nil || returned == paths[0] || !returned.SourceIp.Equal(input.DestinationIp) || !returned.DestinationIp.Equal(input.SourceIp) {
		t.Fatal("unknown-flow fallback lost its separately owned reversed path")
	}
	if !paths[0].SourceIp.Equal(input.SourceIp) || !paths[0].DestinationIp.Equal(input.DestinationIp) || paths[0].SourcePort != input.SourcePort {
		t.Fatal("batch lookup mutated the caller's path")
	}
}

func TestCommittedBatchTcpRstRetiresOnlyAfterDelivery(t *testing.T) {
	for _, version := range []int{4, 6} {
		parent := flowReaperTestParent(context.Background(), DefaultMultiClientSettings())
		parent.log = NewNoopLogger()
		parent.securityPolicy = DisableSecurityPolicy()
		parent.packetStatsCounters = &packetStatsCounters{}
		outbound := flowReaperTestPath(version, IpProtocolTcp, 43003)
		update := newMultiClientChannelUpdate(parent.ctx, outbound)
		defer update.Close()
		client := &multiClientChannel{ctx: parent.ctx}
		update.client.Store(client)
		update.receivedInbound.Store(true)
		parent.flowUpdates[update] = true
		parent.clientUpdates[client] = map[*multiClientChannelUpdate]bool{update: true}
		if version == 4 {
			parent.ip4PathUpdates[outbound.ToIp4Path()] = update
		} else {
			parent.ip6PathUpdates[outbound.ToIp6Path()] = update
		}
		inbound := outbound.ReverseValue()
		inbound.Ack, inbound.Rst = true, true
		packet := ipOosTcpPacket(&inbound, tcpFlagAck|tcpFlagRst, nil)
		delivered := false
		parent.SetReceivePacketsCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, got [][]byte) {
			if update.ctx.Err() != nil || len(got) != 1 || !bytes.Equal(got[0], packet) {
				t.Fatal("TCP RST retired before delivery")
			}
			delivered = true
		})
		parent.clientReceivePackets(client, TransferPath{}, protocol.ProvideMode_Network, TransportTypeUnknown, []*IpPath{&inbound}, [][]byte{packet})
		if !delivered || update.ctx.Err() == nil {
			t.Fatalf("IPv%d delivered reset did not signal completed flow", version)
		}
		// Production retirement wakes the existing shared reaper; it does not
		// synchronously mutate routing maps while delivering the callback.
		retired, _, _ := parent.detachIdleFlows(time.Now())
		if len(retired) != 1 || retired[0].update != update || len(parent.flowUpdates) != 0 || len(parent.clientUpdates) != 0 || len(parent.ip4PathUpdates)+len(parent.ip6PathUpdates) != 0 {
			t.Fatalf("IPv%d delivered reset did not retire exact route and flow ownership", version)
		}
	}
}

func BenchmarkPacketMemoryRegression(b *testing.B) {
	b.Run("ExistingGroup", func(b *testing.B) {
		groups, byKey, path, packet, group := packetGroupAllocationFixture()
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			group.packets, group.ipPaths, group.payloads = group.packets[:0], group.ipPaths[:0], group.payloads[:0]
			group.byteCount = 0
			appendIpPacketGroupBounded(groups, byKey, path, packet[28:], packet, 4, 4096)
		}
	})
	b.Run("CommittedBatch", func(b *testing.B) {
		parent, client, paths, packets := committedBatchAllocationFixture()
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			parent.clientReceivePackets(client, TransferPath{}, protocol.ProvideMode_Network, TransportTypeUnknown, paths, packets)
		}
	})
	b.Run("RawReceive", func(b *testing.B) {
		client, frames, _ := rawReceiveAllocationFixture()
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			client.clientReceive(TransferPath{}, frames, Peer{})
		}
	})
}
