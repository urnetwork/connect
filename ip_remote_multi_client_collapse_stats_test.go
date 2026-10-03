// Packet refusal accounting keeps intentional TCP collapse separate from
// policy blocks and failed admission across the public and mux send paths.
package connect

import (
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A committed sequence forces duplicate refusal without clocks or scheduling.
// Batch counts must describe packets, including the mux's grouped fast path.
func TestMultiClientTcpCollapseCountsRejectedPackets(t *testing.T) {
	for _, mode := range []string{"singular", "batch", "mux"} {
		func() {
			policy := &groupTestSecurityPolicy{stats: DefaultSecurityPolicyStatsCollector()}
			parent, update, closeParent := groupTestParent(t, policy)
			defer closeParent()
			parent.settings.TcpCollapsePrevention = true
			parent.settings.TcpCollapseMaxHold = 0

			providerPacketCount := 0
			admit := true
			update.client.Store(&multiClientChannel{
				ctx:      parent.ctx,
				settings: parent.settings,
				sendGroupForTest: func(group *parsedPacketGroup, _ time.Duration, _ bool) (bool, error) {
					if !admit {
						return false, nil
					}
					for _, packet := range group.packets {
						providerPacketCount++
						MessagePoolReturn(packet.packet)
					}
					return true, nil
				},
			})
			mux := &IpMux{
				upstream:          parent.SendPacket,
				upstreamGroupSend: parent.sendPacketGroup,
			}
			source := SourceId(NewId())
			send := func(packets ...[]byte) int {
				witnesses := groupTestPacketWitnesses(t, packets)
				defer requireGroupTestWitnessesReleased(t, packets, witnesses)
				switch mode {
				case "batch":
					return parent.SendPacketBatch(source, protocol.ProvideMode_Network, packets, 0)
				case "mux":
					return mux.SendPacketBatch(source, protocol.ProvideMode_Network, packets, 0)
				default:
					accepted := 0
					for _, packet := range packets {
						if parent.SendPacket(source, protocol.ProvideMode_Network, packet, 0) {
							accepted++
						} else {
							MessagePoolReturn(packet)
						}
					}
					return accepted
				}
			}
			path := &IpPath{
				Version:         4,
				Protocol:        IpProtocolTcp,
				SourceIp:        net.IPv4(198, 51, 100, 10),
				SourcePort:      32100,
				DestinationIp:   net.IPv4(203, 0, 113, 20),
				DestinationPort: 443,
			}
			packet := func(sequence uint32, payload byte) []byte {
				return MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, sequence, []byte{payload}))
			}
			if got := send(packet(100, 1)); got != 1 {
				t.Fatalf("%s: initial packet accepted=%d, want 1", mode, got)
			}
			if got := parent.TcpCollapseDropCount(); got != 0 {
				t.Fatalf("%s: initial packet counted as collapsed: %d", mode, got)
			}
			duplicates := make([][]byte, 16)
			for i := range duplicates {
				duplicates[i] = packet(100, 1)
			}
			if got := send(duplicates...); got != 0 {
				t.Fatalf("%s: duplicate packets accepted=%d, want 0", mode, got)
			}
			if got := parent.TcpCollapseDropCount(); got != 16 {
				t.Errorf("%s: collapsed packet count=%d, want 16", mode, got)
			}
			if providerPacketCount != 1 {
				t.Errorf("%s: provider received %d packets, want 1", mode, providerPacketCount)
			}
			if got := send(packet(101, 0xff)); got != 0 {
				t.Fatalf("%s: policy-blocked packet accepted=%d, want 0", mode, got)
			}
			if got := parent.PacketStats().BlockEgressPacketCount; got != 1 {
				t.Errorf("%s: policy block count=%d, want 1", mode, got)
			}
			admit = false
			if got := send(packet(101, 1)); got != 0 {
				t.Fatalf("%s: backpressured packet accepted=%d, want 0", mode, got)
			}
			if got := parent.TcpCollapseDropCount(); got != 16 {
				t.Errorf("%s: other refusals changed collapse count to %d, want 16", mode, got)
			}
			admit = true
			if got := send(packet(101, 1)); got != 1 {
				t.Fatalf("%s: new sequence accepted=%d, want 1", mode, got)
			}
			if got := parent.TcpCollapseDropCount(); got != 16 {
				t.Errorf("%s: new sequence changed collapse count to %d, want 16", mode, got)
			}

			// A progressing group carries its redundant members intact. Count
			// only real refusals, not every member considered by the gate.
			wantAccepted := 2
			wantCollapsed := uint64(16)
			if mode == "singular" {
				wantAccepted = 1
				wantCollapsed++
			}
			if got := send(packet(101, 1), packet(102, 1)); got != wantAccepted {
				t.Fatalf("%s: progressing group accepted=%d, want %d", mode, got, wantAccepted)
			}
			if got := parent.TcpCollapseDropCount(); got != wantCollapsed {
				t.Errorf("%s: progressing group collapse count=%d, want %d", mode, got, wantCollapsed)
			}
			ack := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, 103, nil))
			if got := send(ack); got != 0 {
				t.Fatalf("%s: redundant pure ack accepted=%d, want 0", mode, got)
			}
			wantCollapsed++
			if got := parent.TcpCollapseDropCount(); got != wantCollapsed {
				t.Errorf("%s: pure ack collapse count=%d, want %d", mode, got, wantCollapsed)
			}
		}()
	}
}
