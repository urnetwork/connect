package connect

import (
	"bytes"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// Holding the NAT worker proves that final-admission serialization must not
// leave its already-bounded FIFO empty. Releasing it with a full TCP queue
// then proves that pipelining neither skips the earlier packet nor invents
// a Transfer ACK. No upstream TCP connection or WAN ACK exists in this test.
func TestProviderReliableTcpPipelinesNatButPreservesFinalAdmissionOrder(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, budgeted := range []bool{false, true} {
			t.Run(fmt.Sprintf("ipv%d/budget-%t", version, budgeted), func(t *testing.T) {
				testProviderReliableTcpPipeline(t, version, budgeted)
			})
		}
	}
}

func testProviderReliableTcpPipeline(t *testing.T, version int, budgeted bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var budget *TransferMemoryBudget
		if budgeted {
			budget = NewTransferMemoryBudget(mib(2))
			t.Cleanup(func() {
				if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
					t.Error("pipelined NAT/final-TCP ownership leaked its bounded root reservation")
				}
			})
		}
		f := newReliableTcpIngressFixture(t, version, budget)
		gate := make(chan struct{})
		f.nat.beforeSendPacketForTest = func(packet *SendPacket) {
			if packet.reliable != nil {
				select {
				case <-gate:
				case <-f.ctx.Done():
				}
			}
		}
		sequence := NewReceiveSequence(f.ctx, f.client, f.source, NewId(), sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
		ids := []Id{NewId(), NewId()}
		payloads := [][]byte{[]byte("first exact owner"), []byte("second exact owner")}
		for index, payload := range payloads {
			packet := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagAck, uint32(101+index*len(payloads[0])), payload))
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}
			sequence.deliverItems = append(sequence.deliverItems, &receiveItem{
				transferItem:    transferItem{messageId: ids[index], sequenceNumber: uint64(index)},
				receiveCallback: f.client.receiveCallback, ack: true, frames: []*protocol.Frame{frame},
			})
			sequence.deliverFrames = append(sequence.deliverFrames, frame)
		}
		sequence.deliverPeer = Peer{ProvideMode: protocol.ProvideMode_Public}
		go runReliableIngressFixtureDelivery(sequence)
		synctest.Wait()
		if len(f.nat.sendPackets) != 1 {
			t.Error("same-flow final-admission serialization left the available NAT FIFO empty")
		}
		if present, _ := reliableIngressCumulativeHead(sequence); present {
			t.Fatal("NAT queue ownership was mistaken for final TCP admission")
		}
		close(gate)
		synctest.Wait()
		if present, _ := reliableIngressCumulativeHead(sequence); present || len(f.tcp.sendItems) != 1 {
			t.Fatal("full TCP queue was bypassed after NAT admission")
		}
		(<-f.tcp.sendItems).release() // Release the original queued SYN.
		for index, payload := range payloads {
			synctest.Wait()
			select {
			case admitted := <-f.tcp.sendItems:
				if !bytes.Equal(admitted.tcp.payload, payload) {
					t.Errorf("final TCP admission order changed at %d: got %q want %q", index, admitted.tcp.payload, payload)
				}
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != ids[index] {
					t.Errorf("final admission ACK advanced past or lost owner %d: present=%t head=%s", index, present, head.messageId)
				}
				admitted.release()
			default:
				t.Fatalf("retained original %d did not reach the released TCP slot", index)
			}
		}
		synctest.Wait()
		if f.dials.Load() != 0 || sequence.ctx.Err() != nil {
			t.Fatal("local admission depended on a remote socket or closed the receive sequence")
		}
	})
}
