// Committed receive batches charge delivery custody only for TCP and release
// every acquired owner at the actual application callback boundary.
package connect

import (
	"bytes"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// Every packet is parsed normally and its reverse has an established owner.
// The fixture owns no workers; cleanup closes each real flow context.
func committedBatchLeaseFixture(
	t *testing.T,
	version int,
	protocols ...IpProtocol,
) (*RemoteUserNatMultiClient, *multiClientChannel, []*multiClientChannelUpdate, []*IpPath, [][]byte) {
	t.Helper()
	parent := affinityTimeTestParent(t)
	parent.log = NewNoopLogger()
	parent.securityPolicy = DisableSecurityPolicy()
	parent.packetStatsCounters = &packetStatsCounters{}
	client := &multiClientChannel{ctx: parent.ctx, settings: parent.settings}
	updates := make([]*multiClientChannelUpdate, len(protocols))
	paths := make([]*IpPath, len(protocols))
	packets := make([][]byte, len(protocols))
	parent.clientUpdates[client] = map[*multiClientChannelUpdate]bool{}
	for index, ipProtocol := range protocols {
		outbound := affinityTimeTestPath(version, 44100+index)
		inbound := outbound.ReverseValue()
		switch ipProtocol {
		case IpProtocolTcp:
			packets[index] = ipOosTcpPacket(&inbound, tcpFlagAck, nil)
		case IpProtocolUdp:
			packets[index] = testingUdpPacket(version, inbound.SourceIp.String(), inbound.DestinationIp.String(), inbound.DestinationPort, []byte("synthetic return"))
		case IpProtocolIcmp:
			if version == 4 {
				packets[index] = testingIcmp4EchoPacket(inbound.SourceIp.String(), inbound.DestinationIp.String(), 42, 1, []byte("synthetic echo"))
			} else {
				packets[index] = testingIcmp6EchoPacket(inbound.SourceIp.String(), inbound.DestinationIp.String(), 42, 1, []byte("synthetic echo"))
			}
		default:
			t.Fatal("unsupported fixture protocol")
		}
		path, err := ParseIpPath(packets[index])
		if err != nil || path.Protocol != ipProtocol {
			t.Fatalf("IPv%d %s fixture did not parse: %v", version, ipProtocol, err)
		}
		paths[index] = path
		reversed := path.ReverseValue()
		update := newMultiClientChannelUpdate(parent.ctx, &reversed)
		update.client.Store(client)
		update.receivedInbound.Store(true)
		updates[index] = update
		parent.flowUpdates[update] = true
		parent.clientUpdates[client][update] = true
		if version == 4 {
			parent.ip4PathUpdates[reversed.ToIp4Path()] = update
		} else {
			parent.ip6PathUpdates[reversed.ToIp6Path()] = update
		}
	}
	return parent, client, updates, paths, packets
}

// One backing slice belongs to delivery. Non-TCP bookkeeping must allocate
// nothing on any repetition, including ICMP and both address families.
func TestCommittedBatchNonTcpLeaseDoesNotAllocate(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, ipProtocol := range []IpProtocol{IpProtocolUdp, IpProtocolIcmp} {
			parent, client, updates, paths, packets := committedBatchLeaseFixture(t, version, ipProtocol)
			deliveries := 0
			parent.SetReceivePacketsCallback(func(_ TransferPath, _ protocol.ProvideMode, path *IpPath, received [][]byte) {
				if path != nil || len(received) != 1 || !bytes.Equal(received[0], packets[0]) {
					panic("committed batch changed its borrowed packet")
				}
				deliveries++
			})
			allocs := testing.AllocsPerRun(1000, func() {
				parent.clientReceivePackets(client, TransferPath{}, protocol.ProvideMode_Network, TransportTypeUnknown, paths, packets)
			})
			if allocs != 1 {
				t.Errorf("IPv%d %s batch allocated %.0f objects, want only 1 delivery slice", version, ipProtocol, allocs)
			}
			if deliveries != 1001 || len(parent.affinityDeliveries) != 0 || updates[0].affinityDelivery.Load() != 0 {
				t.Errorf("IPv%d %s changed delivery count or retained non-TCP custody", version, ipProtocol)
			}
		}
	}
}

// The callback itself observes both TCP owners. Returning, panicking, and
// canceling all preserve custody through that callback and then release it.
func testCommittedBatchMixedLeaseRelease(t *testing.T, outcome string) {
	t.Helper()
	for _, version := range []int{4, 6} {
		parent, client, updates, paths, packets := committedBatchLeaseFixture(t, version,
			IpProtocolUdp, IpProtocolTcp, IpProtocolIcmp, IpProtocolTcp)
		called := false
		parent.SetReceivePacketsCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, received [][]byte) {
			called = true
			if len(received) != len(packets) || len(parent.affinityDeliveries) != 2 {
				t.Errorf("IPv%d mixed batch lost packets or exact TCP owners", version)
			}
			for index, update := range updates {
				want := int64(0)
				if paths[index].Protocol == IpProtocolTcp {
					want = 1
				}
				if update.affinityDelivery.Load() != want || !bytes.Equal(received[index], packets[index]) {
					t.Errorf("IPv%d mixed member %d changed custody or order", version, index)
				}
			}
			switch outcome {
			case "panic":
				panic("synthetic batch callback panic")
			case "cancel":
				parent.cancel()
				if len(parent.affinityDeliveries) != 2 {
					t.Error("cancellation released the executing batch")
				}
				lease, admitted := parent.acquireAffinityDelivery(updates[1].ipPath, updates[1])
				if admitted {
					lease.release()
					t.Error("canceled parent admitted another TCP delivery")
				}
			}
		})
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			parent.clientReceivePackets(client, TransferPath{}, protocol.ProvideMode_Network, TransportTypeUnknown, paths, packets)
		}()
		if !called || (outcome == "panic") != (recovered != nil) {
			t.Errorf("IPv%d callback outcome=%s recovered=%v called=%v", version, outcome, recovered, called)
		}
		if len(parent.affinityDeliveries) != 0 {
			t.Errorf("IPv%d batch return retained delivery owners", version)
		}
		for _, update := range updates {
			if update.affinityDelivery.Load() != 0 {
				t.Errorf("IPv%d batch return retained a flow lease", version)
			}
		}
	}
}

func TestCommittedBatchMixedLeaseRelease(t *testing.T) {
	testCommittedBatchMixedLeaseRelease(t, "return")
}

func TestCommittedBatchMixedLeasePanicRelease(t *testing.T) {
	testCommittedBatchMixedLeaseRelease(t, "panic")
}

func TestCommittedBatchMixedLeaseCancellationRelease(t *testing.T) {
	testCommittedBatchMixedLeaseRelease(t, "cancel")
}
