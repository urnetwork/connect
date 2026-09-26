package connect

import (
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func batchResultTestUdpPacket(path *IpPath, payload []byte) []byte {
	return MessagePoolCopy(ipOosUdpPacket(path, payload))
}

// Exercise the real grouping, policy and collapse paths. Only the final
// provider admission is replaced; no sockets, database or devices are used.
func TestMultiClientBatchResultsMixedTcpUdp(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		for _, detailed := range []bool{false, true} {
			t.Run(fmtBatchResultCase(reversed, detailed), func(t *testing.T) {
				parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
				defer closeParent()
				parent.settings.TcpCollapsePrevention = true
				parent.settings.TcpCollapseMaxHold = 0
				udpUpdate := newMultiClientChannelUpdate(parent.ctx, nil)
				defer udpUpdate.Close()
				providerTCP, providerUDP := 0, 0
				channel := &multiClientChannel{
					ctx: parent.ctx, settings: parent.settings,
					sendGroupForTest: func(group *parsedPacketGroup, _ time.Duration, _ bool) (bool, error) {
						for _, member := range group.packets {
							if member.ipPath.Protocol == IpProtocolTcp {
								providerTCP++
							} else {
								providerUDP++
							}
							MessagePoolReturn(member.packet)
						}
						return true, nil
					},
				}
				update.client.Store(channel)
				udpUpdate.client.Store(channel)
				parent.sendClientPathForTest = func(path *IpPath, _ flowPin, send func(*multiClientChannelUpdate, *multiClientChannel)) {
					flow := update
					if path.Protocol == IpProtocolUdp {
						flow = udpUpdate
					}
					flow.ipPath = path
					send(flow, channel)
				}
				tcpPath := &IpPath{
					Version: 4, Protocol: IpProtocolTcp,
					SourceIp: net.IPv4(198, 51, 100, 10), SourcePort: 32100,
					DestinationIp: net.IPv4(203, 0, 113, 20), DestinationPort: 443,
				}
				tcpPacket := func() []byte {
					return MessagePoolCopy(ipOosTcpPacketSequence(tcpPath, tcpFlagAck, 100, []byte{1}))
				}
				source := SourceId(NewId())
				first := [][]byte{tcpPacket()}
				witnesses := groupTestPacketWitnesses(t, first)
				if parent.SendPacketBatch(source, protocol.ProvideMode_Network, first, 0) != 1 {
					t.Fatal("prime TCP collapse state")
				}
				requireGroupTestWitnessesReleased(t, first, witnesses)
				packets := [][]byte{tcpPacket(), batchResultTestUdpPacket(udpTestPath(4), []byte{2})}
				want := []bool{false, true}
				if reversed {
					packets[0], packets[1] = packets[1], packets[0]
					want[0], want[1] = want[1], want[0]
				}
				witnesses = groupTestPacketWitnesses(t, packets)
				defer requireGroupTestWitnessesReleased(t, packets, witnesses)
				var count int
				if detailed {
					accepted := []bool{true, true} // stale caller values must be cleared
					count = parent.SendPacketBatchWithResults(source, protocol.ProvideMode_Network, packets, 0, accepted)
					if !reflect.DeepEqual(accepted, want) {
						t.Fatalf("accepted=%v want=%v", accepted, want)
					}
				} else {
					count = parent.SendPacketBatch(source, protocol.ProvideMode_Network, packets, 0)
				}
				if count != 1 || providerTCP != 1 || providerUDP != 1 || parent.TcpCollapseDropCount() != 1 {
					t.Fatalf("accepted=%d provider TCP/UDP=%d/%d collapse=%d", count, providerTCP, providerUDP, parent.TcpCollapseDropCount())
				}
			})
		}
	}
}

func fmtBatchResultCase(reversed, detailed bool) string {
	name := "tcp_udp/legacy"
	if reversed {
		name = "udp_tcp/legacy"
	}
	if detailed {
		name += "_with_results"
	}
	return name
}

// Non-contiguous members retain their original positions, even when one
// exact flow is split into several bounded groups with different outcomes.
func TestMultiClientBatchResultsPreserveInputIndexes(t *testing.T) {
	for _, groupLimit := range []int{1, 2, 64} {
		parent, update, closeParent := groupTestParent(t, &groupTestSecurityPolicy{stats: DefaultSecurityPolicyStatsCollector()})
		parent.settings.PacketGroupMaxPacketCount = groupLimit
		providerCalls := 0
		update.client.Store(&multiClientChannel{
			ctx: parent.ctx, settings: parent.settings,
			sendGroupForTest: func(group *parsedPacketGroup, _ time.Duration, _ bool) (bool, error) {
				providerCalls++
				for _, member := range group.packets {
					MessagePoolReturn(member.packet)
				}
				return true, nil
			},
		})
		other := *udpTestPath(4)
		other.SourcePort++
		packets := [][]byte{
			batchResultTestUdpPacket(udpTestPath(4), []byte{1}),
			batchResultTestUdpPacket(&other, []byte{2}),
			MessagePoolCopy([]byte{0}), // malformed incidental input
			batchResultTestUdpPacket(udpTestPath(4), []byte{0xff}),
			batchResultTestUdpPacket(udpTestPath(4), []byte{3}),
		}
		want := []bool{false, true, false, false, false}
		wantCalls := 1
		if groupLimit == 1 {
			want = []bool{true, true, false, false, true}
			wantCalls = 3
		} else if groupLimit == 2 {
			want = []bool{false, true, false, false, true}
			wantCalls = 2
		}
		witnesses := groupTestPacketWitnesses(t, packets)
		accepted := []bool{true, true, true, true, true}
		count := parent.SendPacketBatchWithResults(SourceId(NewId()), protocol.ProvideMode_Network, packets, 0, accepted)
		requireGroupTestWitnessesReleased(t, packets, witnesses)
		closeParent()
		if !reflect.DeepEqual(accepted, want) || count != wantCalls || providerCalls != wantCalls {
			t.Fatalf("group limit=%d accepted=%v count/calls=%d/%d want=%v/%d", groupLimit, accepted, count, providerCalls, want, wantCalls)
		}
	}
}

func TestMultiClientBatchResultsRejectedAdmission(t *testing.T) {
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	update.client.Store(&multiClientChannel{
		ctx: parent.ctx, settings: parent.settings,
		sendGroupForTest: func(*parsedPacketGroup, time.Duration, bool) (bool, error) { return false, nil },
	})
	packets := [][]byte{batchResultTestUdpPacket(udpTestPath(4), []byte{1})}
	witnesses := groupTestPacketWitnesses(t, packets)
	defer requireGroupTestWitnessesReleased(t, packets, witnesses)
	accepted := []bool{true}
	if count := parent.SendPacketBatchWithResults(SourceId(NewId()), protocol.ProvideMode_Network, packets, 0, accepted); count != 0 || accepted[0] {
		t.Fatalf("refused UDP count=%d accepted=%v", count, accepted)
	}
}

// A fragment's result follows the existing gate-ownership contract, and
// complete runs on either side still use absolute caller input positions.
func TestMultiClientBatchResultsFragmentRuns(t *testing.T) {
	parent, _, closeParent := groupTestParent(t, &groupTestSecurityPolicy{stats: DefaultSecurityPolicyStatsCollector()})
	defer closeParent()
	defer parent.egressIpv4Fragments.close()
	complete := batchResultTestUdpPacket(udpTestPath(4), append([]byte{0xff}, make([]byte, 1199)...))
	fragments, err := fragmentIpv4Packet(complete, DefaultMtu)
	if err != nil || len(fragments) != 2 {
		t.Fatalf("fragment fixture count=%d err=%v", len(fragments), err)
	}
	packets := [][]byte{
		batchResultTestUdpPacket(udpTestPath(4), []byte{0xff}), fragments[0],
		MessagePoolCopy([]byte{0}), fragments[1],
		batchResultTestUdpPacket(udpTestPath(4), []byte{0xff}),
	}
	witnesses := groupTestPacketWitnesses(t, packets)
	defer requireGroupTestWitnessesReleased(t, packets, witnesses)
	accepted := make([]bool, len(packets))
	count := parent.SendPacketBatchWithResults(SourceId(NewId()), protocol.ProvideMode_Network, packets, 0, accepted)
	want := []bool{false, true, false, true, false}
	if count != 2 || !reflect.DeepEqual(accepted, want) {
		t.Fatalf("fragment gate count=%d accepted=%v want=%v", count, accepted, want)
	}
}

func TestMultiClientBatchResultsLengthMismatchKeepsOwnership(t *testing.T) {
	parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	packet := batchResultTestUdpPacket(udpTestPath(4), []byte{1})
	defer func() {
		if recover() == nil {
			t.Error("invalid result length did not panic before consuming inputs")
		}
		if !MessagePoolReturn(packet) {
			t.Error("invalid call changed packet ownership")
		}
	}()
	parent.SendPacketBatchWithResults(SourceId(NewId()), protocol.ProvideMode_Network, [][]byte{packet}, 0, nil)
}

type batchResultOrderedPolicy struct {
	SecurityPolicy
	calls int
}

func (p *batchResultOrderedPolicy) InspectEgress(protocol.ProvideMode, *IpPath, []byte) (SecurityPolicyResult, error) {
	p.calls++
	if p.calls == 2 {
		return SecurityPolicyResultDrop, nil
	}
	return SecurityPolicyResultAllow, nil
}

// SMTP's intentionally ordered singular branch can partially accept one
// flow group; do not promote that partial result to every group member.
func TestMultiClientBatchResultsOrderedSmtpMembers(t *testing.T) {
	policy := &batchResultOrderedPolicy{SecurityPolicy: DisableSecurityPolicy()}
	parent, update, closeParent := groupTestParent(t, policy)
	defer closeParent()
	providerCount := 0
	update.client.Store(&multiClientChannel{
		ctx: parent.ctx, settings: parent.settings,
		sendGroupForTest: func(group *parsedPacketGroup, _ time.Duration, _ bool) (bool, error) {
			providerCount += len(group.packets)
			for _, packet := range group.packets {
				MessagePoolReturn(packet.packet)
			}
			return true, nil
		},
	})
	path := smtpTestPath(47001, smtpImplicitTlsPort, 100)
	packets := [][]byte{
		MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 100, nil)),
		MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 101, nil)),
	}
	witnesses := groupTestPacketWitnesses(t, packets)
	defer requireGroupTestWitnessesReleased(t, packets, witnesses)
	accepted := []bool{false, true}
	count := parent.SendPacketBatchWithResults(SourceId(NewId()), protocol.ProvideMode_Network, packets, 0, accepted)
	if count != 1 || providerCount != 1 || !reflect.DeepEqual(accepted, []bool{true, false}) {
		t.Fatalf("ordered SMTP count/provider=%d/%d accepted=%v", count, providerCount, accepted)
	}
}

// The count-only API must not pay the opt-in per-input indexing allocation.
// This is the previous grouping loop, using the same parser/group allocator.
func batchResultLegacyGrouping(packets [][]byte) (groups []*ipPacketGroup, rejected [][]byte) {
	groupsByKey := map[ipPacketFlowKey]*ipPacketGroup{}
	for _, packet := range packets {
		var path IpPath
		payload, err := parseIpPathWithPayloadBorrowed(packet, &path)
		if err != nil || !appendIpPacketGroupBounded(&groups, groupsByKey, &path, payload, packet, 64, 96*1024) {
			rejected = append(rejected, packet)
		}
	}
	return
}

func TestMultiClientBatchResultsLegacyGroupingAllocations(t *testing.T) {
	packets := make([][]byte, 64)
	for i := range packets {
		path := *udpTestPath(4)
		path.SourcePort += i % 4
		packets[i] = ipOosUdpPacket(&path, []byte{1})
	}
	measure := func(group func([][]byte) ([]*ipPacketGroup, [][]byte)) float64 {
		return testing.AllocsPerRun(100, func() {
			groups, rejected := group(packets)
			if len(groups) != 4 || len(rejected) != 0 {
				t.Fatal("allocation fixture grouping changed")
			}
		})
	}
	before := measure(batchResultLegacyGrouping)
	after := measure(func(packets [][]byte) ([]*ipPacketGroup, [][]byte) {
		return groupIpPacketsBounded(packets, 64, 96*1024)
	})
	if after != before {
		t.Fatalf("count-only grouping allocations=%g, prior loop=%g", after, before)
	}
	t.Logf("count-only grouping allocations unchanged: %g per 64-packet burst", after)
}
