// Producer metadata must follow the actual offered IP bytes through raw,
// wrapped and coalesced sends without changing ACK or pooled-root custody.
package connect

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The existing paused real SendSequence exposes the producer's exact offer;
// downstream budget exemption tests independently exercise serialization.
func ipProtocolProviderOffer(t *testing.T, version int, defaultAck ...bool) (*RemoteUserNatProvider, *SendSequence, TransferPath, TransferKey) {
	t.Helper()
	settings := DefaultRemoteUserNatProviderSettings()
	settings.ProtocolVersion = version
	settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	clientSettings := DefaultClientSettings()
	clientSettings.Log = NewNoopLogger()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.DefaultTransferOpts.Ack = false
	if len(defaultAck) != 0 {
		clientSettings.DefaultTransferOpts.Ack = defaultAck[0]
	}
	provider, client, _ := newProviderTransferKeyTestFixtureWithClientSettings(t, settings, clientSettings)
	t.Cleanup(func() {
		provider.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	})
	source := SourceId(NewId())
	key := TransferKey{ForceStream: true, EncryptionRole: protocol.SequenceRole_SequenceRoleServer}
	id := sendSequenceId{Destination: source.SourceId, ForceStream: true, CompanionContract: true, EncryptionRole: sequenceTlsRoleServer}
	sequence := installProviderReturnTestSequence(t, provider, client, id)
	// A buffered source now has one ordered FIFO for both admission classes.
	// This paused producer seam observes its actual cross-class offer order.
	sequence.packs = make(chan *SendPack, 8)
	sequence.protocolPacks = nil
	t.Cleanup(func() {
		for _, offers := range []chan *SendPack{sequence.packs, sequence.protocolPacks} {
			for len(offers) != 0 {
				pack := <-offers
				pack.returnFrames()
				pack.releaseRaw()
			}
		}
	})
	return provider, sequence, source, key
}

// Complete documentation-address TCP packets, including FIN/SYN/RST, keep
// their original byte representation at every test boundary.
func ipProtocolOfferPacket(flags byte, payload []byte) []byte {
	return ipOosTcpPacketSequence(&IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.ParseIP("192.0.2.10"), DestinationIp: net.ParseIP("198.51.100.20"), SourcePort: 41000, DestinationPort: 443}, flags, 100, payload)
}

func ipProtocolTakeOffer(t *testing.T, sequence *SendSequence, protocolOnly bool) *SendPack {
	t.Helper()
	offers := sequence.packs
	if protocolOnly && sequence.protocolPacks != nil {
		offers = sequence.protocolPacks
	}
	select {
	case pack := <-offers:
		return pack
	default:
		t.Fatal("real producer did not admit its exact offer")
		return nil
	}
}

func TestIpProviderSingleOfferMarksOnlyZeroPayloadTcpControl(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		provider, sequence, source, key := ipProtocolProviderOffer(t, version)
		for _, control := range []struct {
			flags   byte
			payload []byte
			want    bool
		}{{tcpFlagAck, nil, true}, {tcpFlagSyn, nil, true}, {tcpFlagFin | tcpFlagAck, nil, true}, {tcpFlagRst | tcpFlagAck, nil, true}, {tcpFlagAck, []byte("application"), false}} {
			raw := ipProtocolOfferPacket(control.flags, control.payload)
			item := &providerReturnItem{source: source, transferKey: providerReplyTransferKey(key, protocol.ProvideMode_Public), provideMode: protocol.ProvideMode_Public, ipProtocol: IpProtocolTcp, packet: MessagePoolCopy(raw), packetByteCount: ByteCount(len(raw))}
			if !provider.sendReturnPacket(item) {
				t.Fatalf("v%d return offer refused", version)
			}
			pack := ipProtocolTakeOffer(t, sequence, control.want)
			packet, err := ipPacketFromProviderBytes(pack.Frame)
			if err != nil || !bytes.Equal(packet, raw) || pack.NoBudget != control.want || !pack.Ack || !pack.ForceStream || !pack.CompanionContract || pack.NetworkPeer {
				t.Errorf("v%d flags=%x changed packet/class/reliable options: %+v %v", version, control.flags, pack.TransferOptions, err)
			}
			pack.returnFrames()
			pack.releaseRaw()
		}
	}
}

func TestIpProviderBatchOfferKeepsMixedDataBudgeted(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		for _, test := range []struct {
			name       string
			packets    [][]byte
			groupSizes []int
			noBudget   []bool
		}{
			{
				name: "control-prefix-and-data",
				packets: [][]byte{ipProtocolOfferPacket(tcpFlagAck, nil), ipProtocolOfferPacket(tcpFlagFin|tcpFlagAck, nil),
					ipProtocolOfferPacket(tcpFlagAck, nil), ipProtocolOfferPacket(tcpFlagAck, []byte("data"))},
				groupSizes: []int{2, 1, 1}, noBudget: []bool{true, true, false},
			},
			{
				name: "alternating-with-payload-fin-and-malformed",
				packets: [][]byte{ipProtocolOfferPacket(tcpFlagAck, []byte("first")), ipProtocolOfferPacket(tcpFlagAck, nil),
					ipProtocolOfferPacket(tcpFlagFin|tcpFlagAck, []byte("last")), ipProtocolOfferPacket(tcpFlagRst|tcpFlagAck, nil), {0x45}},
				groupSizes: []int{1, 1, 1, 1, 1}, noBudget: []bool{false, true, false, true, false},
			},
			{
				name: "all-controls",
				packets: [][]byte{ipProtocolOfferPacket(tcpFlagAck, nil), ipProtocolOfferPacket(tcpFlagSyn, nil),
					ipProtocolOfferPacket(tcpFlagFin|tcpFlagAck, nil), ipProtocolOfferPacket(tcpFlagRst|tcpFlagAck, nil)},
				groupSizes: []int{2, 2}, noBudget: []bool{true, true},
			},
			{
				name: "payload-controls-stay-data",
				packets: [][]byte{ipProtocolOfferPacket(tcpFlagSyn, []byte("first")), ipProtocolOfferPacket(tcpFlagAck, []byte("middle")),
					ipProtocolOfferPacket(tcpFlagFin|tcpFlagAck, []byte("last"))},
				groupSizes: []int{2, 1}, noBudget: []bool{false, false},
			},
		} {
			provider, sequence, source, key := ipProtocolProviderOffer(t, version)
			owned := make([][]byte, len(test.packets))
			witnesses := make([][]byte, len(test.packets))
			var total ByteCount
			for index, packet := range test.packets {
				owned[index] = MessagePoolCopy(packet)
				witnesses[index] = MessagePoolShareReadOnly(owned[index])
				total += ByteCount(len(packet))
			}
			t.Cleanup(func() {
				for _, packet := range witnesses {
					if packet != nil {
						MessagePoolReturn(packet)
					}
				}
			})
			item := &providerReturnItem{source: source, transferKey: providerReplyTransferKey(key, protocol.ProvideMode_Public),
				provideMode: protocol.ProvideMode_Public, ipProtocol: IpProtocolTcp, packets: owned, packetByteCount: total}
			if !provider.sendReturnBatchWithLimits(item, 2, providerReturnBatchMaxBytes) || item.packets != nil {
				t.Fatalf("v%d %s: real batch offer refused or left caller ownership behind", version, test.name)
			}
			packetIndex := 0
			for groupIndex, size := range test.groupSizes {
				want := test.noBudget[groupIndex]
				pack := ipProtocolTakeOffer(t, sequence, want)
				func() {
					defer pack.releaseRaw()
					defer pack.returnFrames()
					if len(pack.Frames) != size || pack.NoBudget != want || !pack.Ack || !pack.CompanionContract || !pack.ForceStream ||
						pack.NetworkPeer || pack.EncryptionRole != sequenceTlsRoleServer || pack.EncryptionCompanion || pack.Destination != source.SourceId {
						t.Fatalf("v%d %s group%d changed contiguous class/reliable/key options: frames=%d options=%+v", version, test.name, groupIndex, len(pack.Frames), pack.TransferOptions)
					}
					for _, frame := range pack.Frames {
						packet, err := ipPacketFromProviderBytes(frame)
						if err != nil || packetIndex >= len(test.packets) || !bytes.Equal(packet, test.packets[packetIndex]) {
							t.Fatalf("v%d %s: contiguous class partition changed original packet order or bytes", version, test.name)
						}
						packetIndex++
					}
				}()
			}
			if packetIndex != len(test.packets) || len(sequence.packs) != 0 || sequence.protocolPacks != nil {
				t.Fatalf("v%d %s: real return batching omitted, duplicated or hid an original in another class queue", version, test.name)
			}
			for index, packet := range witnesses {
				if !MessagePoolReturn(packet) {
					t.Errorf("v%d %s original%d survived disposal of every actual offered group", version, test.name, index)
				}
				witnesses[index] = nil
				if owned[index] != nil {
					t.Errorf("v%d %s: return item retained a transferred original slot", version, test.name)
				}
			}
		}
	}
}

func TestIpLocalClientOfferMarksControlWithoutChangingDefaultAck(t *testing.T) {
	assertMessagePoolOwnership(t)
	provider, _, source, _ := ipProtocolProviderOffer(t, 2, true)
	client := provider.client
	// The provider helper's server lane is deliberately separate. This basic
	// client offer follows the ordinary client-role, destination-only lane.
	id := sendSequenceId{Destination: source.SourceId, EncryptionRole: sequenceTlsRoleClient}
	clientSequence := installProviderReturnTestSequence(t, provider, client, id)
	clientSequence.protocolPacks = make(chan *SendPack, 1)
	t.Cleanup(func() {
		for _, offers := range []chan *SendPack{clientSequence.packs, clientSequence.protocolPacks} {
			for len(offers) != 0 {
				pack := <-offers
				pack.returnFrames()
				pack.releaseRaw()
			}
		}
	})
	remote := &RemoteUserNatClient{client: client, securityPolicy: DisableSecurityPolicyWithStats(client.Ctx(), DefaultSecurityPolicyStatsCollector()), pathTable: newPathTable([]MultiHopId{RequireMultiHopId(source.SourceId)}), provideMode: protocol.ProvideMode_Network}
	for _, payload := range [][]byte{nil, []byte("data")} {
		raw := ipProtocolOfferPacket(tcpFlagAck, payload)
		packet := MessagePoolCopy(raw)
		if !remote.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
			MessagePoolReturn(packet)
			t.Fatal("basic client offer refused")
		}
		pack := ipProtocolTakeOffer(t, clientSequence, len(payload) == 0)
		got, err := ipPacketToProviderBytes(pack.Frame)
		if err != nil || !bytes.Equal(got, raw) || pack.NoBudget != (len(payload) == 0) || pack.Ack != client.settings.DefaultTransferOpts.Ack {
			t.Errorf("client changed packet/class/default ACK: %+v %v", pack.TransferOptions, err)
		}
		pack.returnFrames()
		pack.releaseRaw()
	}
}

func TestNatProtocolPacketAdmissionUsesCountWithoutAnyByteReserve(t *testing.T) {
	assertMessagePoolOwnership(t)
	budget := NewTransferMemoryBudget(1)
	if !budget.TryReserve(1) {
		t.Fatal("full budget premise")
	}
	defer budget.Release(1)
	nat := &LocalUserNat{settings: &LocalUserNatSettings{MemoryBudget: budget}}
	for _, flags := range []byte{tcpFlagAck, tcpFlagSyn, tcpFlagFin | tcpFlagAck, tcpFlagRst | tcpFlagAck} {
		packet := ipProtocolOfferPacket(flags, nil)
		before := budget.Stats()
		memory, admitted := nat.reserveSendPacketsMemory([][]byte{packet})
		if !admitted || memory.budget != nil || memory.bytes != 0 || budget.Stats() != before {
			t.Fatal("protocol packet acquired DATA or reserved-control bytes")
		}
	}
	controlPacket := ipProtocolOfferPacket(tcpFlagAck, nil)
	boundary := make([][]byte, providerReturnBatchMaxFrames)
	for index := range boundary {
		boundary[index] = controlPacket
	}
	if memory, ok := nat.reserveSendPacketsMemory(boundary); !ok || memory.budget != nil {
		t.Fatal("existing16-frame count boundary refused")
	}
	if _, ok := nat.reserveSendPacketsMemory(append(boundary, controlPacket)); ok {
		t.Fatal("oversized public protocol group lost its owner-count bound")
	}
	data := ipProtocolOfferPacket(tcpFlagAck, []byte("x"))
	if _, ok := nat.reserveSendPacketsMemory([][]byte{controlPacket, data}); ok {
		t.Fatal("mixed ACK plus payload bypassed DATA admission")
	}
}

// The public APIs retain their distinct contracts: false leaves SendPackets
// inputs owned by the caller, while SendPacketBatch consumes rejected inputs.
func TestNatProtocolPublicBatchKeepsQueueCountAndCancellationOwnership(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	budget := NewTransferMemoryBudget(1)
	if !budget.TryReserve(1) {
		t.Fatal("full DATA premise")
	}
	t.Cleanup(func() { budget.Release(1) })
	nat := &LocalUserNat{ctx: ctx, cancel: cancel, settings: &LocalUserNatSettings{MemoryBudget: budget}, sendPackets: make(chan *SendPacket, 1)}
	drain := func() {
		for {
			select {
			case queued := <-nat.sendPackets:
				for _, packet := range queued.packets {
					MessagePoolReturn(packet)
				}
				queued.finish()
			default:
				return
			}
		}
	}
	t.Cleanup(drain)
	input := func(count int) ([][]byte, *bool) {
		packets := make([][]byte, count)
		for index := range packets {
			packets[index] = MessagePoolCopy(ipProtocolOfferPacket(tcpFlagAck, nil))
		}
		consumed := new(bool)
		t.Cleanup(func() {
			if !*consumed {
				for _, packet := range packets {
					MessagePoolReturn(packet)
				}
			}
		})
		return packets, consumed
	}
	source := SourceId(NewId())
	before := budget.Stats()
	oversized, oversizedConsumed := input(providerReturnBatchMaxFrames + 1)
	*oversizedConsumed = nat.SendPackets(source, protocol.ProvideMode_Network, oversized, 0)
	if *oversizedConsumed || len(nat.sendPackets) != 0 {
		t.Fatal("oversized protocol batch consumed inputs or entered the NAT")
	}
	boundary, boundaryConsumed := input(providerReturnBatchMaxFrames)
	*boundaryConsumed = nat.SendPackets(source, protocol.ProvideMode_Network, boundary, 0)
	if !*boundaryConsumed || len(nat.sendPackets) != 1 || budget.Stats() != before {
		t.Fatal("exact count boundary used byte credit or failed admission")
	}
	blocked, blockedConsumed := input(1)
	*blockedConsumed = nat.SendPackets(source, protocol.ProvideMode_Network, blocked, 0)
	if *blockedConsumed {
		t.Fatal("protocol exemption bypassed the bounded NAT queue")
	}
	drain()
	batch, batchConsumed := input(1)
	accepted := nat.SendPacketBatch(source, protocol.ProvideMode_Network, batch, 0)
	*batchConsumed = true
	if accepted != 1 || len(nat.sendPackets) != 1 || budget.Stats() != before {
		t.Fatal("packet-batch metadata reapplied the DATA byte gate")
	}
	// The API borrows list metadata even though it consumes accepted roots.
	packet := batch[0]
	batch[0] = nil
	queued := <-nat.sendPackets
	if len(queued.packets[0]) == 0 {
		t.Error("public packet-batch list was retained without its own copy")
	}
	MessagePoolReturn(packet)
	queued.finish()
	cancel()
	canceled, canceledConsumed := input(1)
	*canceledConsumed = nat.SendPackets(source, protocol.ProvideMode_Network, canceled, 0)
	if *canceledConsumed || len(nat.sendPackets) != 0 || budget.Stats() != before {
		t.Fatal("cancellation consumed caller roots or acquired byte credit")
	}
}
