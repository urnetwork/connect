package connect

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func TestNatPacketReservationSplitAndMove(t *testing.T) {
	root := NewTransferMemoryBudget(64)
	source := NewTransferMemoryBudgetWithParent(64, root)
	target := NewTransferMemoryBudgetWithParent(32, root)
	owner, ok := reserveNatMemory(source, 64)
	if !ok {
		t.Fatal("fixture reserve")
	}
	packet, ok := owner.split(32)
	if !ok || owner.bytes != 32 || packet.bytes != 32 || !packet.moveTo(target) || root.Available() != 0 {
		t.Fatal("prepaid packet split/move lost shared-root ownership")
	}
	if _, ok := owner.split(33); ok || owner.moveTo(target) {
		t.Fatal("failed split or target ceiling changed owner")
	}
	packet.release()
	packet.release()
	owner.release()
	for _, budget := range []*TransferMemoryBudget{source, target, root} {
		if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
			t.Fatal("packet credit was not released exactly once")
		}
	}
}

// Exercises the real final TcpBuffer -> TcpSequence.send boundary. A full
// parent is harmless when the packet already owns its admission credit.
// Refusal/cancel retain the caller's exact claim and bytes for retry.
func TestTcpPrepaidFinalAdmission(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, scenario := range []string{"success", "queue-full", "canceled", "wrong-child", "small-control-partition"} {
			t.Run(fmt.Sprintf("ipv%d/%s", version, scenario), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				root := NewTransferMemoryBudget(8192)
				budget := NewTransferMemoryBudgetWithParent(8192, root)
				sequence := newEstablishedTcpAckFastPathSequence(t)
				sequence.sendItems = make(chan *TcpSendItem, 1)
				settings := DefaultTcpBufferSettings()
				settings.MemoryBudget = budget
				settings.Log = NewNoopLogger()
				path, source := icmpTcpTestPath(version), SourceId(NewId())
				payload := []byte("prepaid final queue owner")
				if scenario == "small-control-partition" {
					payload = nil
				}
				packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, 101, payload))
				var tcp parsedTcp
				var send func(*natMemoryReservation) (bool, error)
				if version == 4 {
					_, sourceIP, destinationIP, transport, ok := parseIpv4(packet)
					if !ok || !parseTcpPacket(sourceIP, destinationIP, transport, &tcp) {
						t.Fatal("fixture parse4")
					}
					buffer := NewTcp4Buffer(sequence.ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, settings)
					buffer.sequences[NewBufferId4(source, tcp.sourceIp, int(tcp.sourcePort), tcp.destinationIp, int(tcp.destinationPort))] = sequence
					send = func(credit *natMemoryReservation) (bool, error) {
						return buffer.sendTransferKey(source, TransferKey{}, protocol.ProvideMode_Public, &tcp, 0, packet, credit)
					}
				} else {
					_, sourceIP, destinationIP, transport, ok := parseIpv6(packet)
					if !ok || !parseTcpPacket(sourceIP, destinationIP, transport, &tcp) {
						t.Fatal("fixture parse6")
					}
					buffer := NewTcp6Buffer(sequence.ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, settings)
					buffer.sequences[NewBufferId6(source, tcp.sourceIp, int(tcp.sourcePort), tcp.destinationIp, int(tcp.destinationPort))] = sequence
					send = func(credit *natMemoryReservation) (bool, error) {
						return buffer.sendTransferKey(source, TransferKey{}, protocol.ProvideMode_Public, &tcp, 0, packet, credit)
					}
				}
				creditBudget := budget
				if scenario == "wrong-child" || scenario == "small-control-partition" {
					creditBudget = NewTransferMemoryBudgetWithParent(8192, root)
				}
				credit, ok := reserveNatMemory(creditBudget, natPacketMemoryByteCount(packet))
				if !ok {
					t.Fatal("fixture packet credit")
				}
				original := credit
				fill := root.Available()
				if !root.TryReserve(fill) {
					t.Fatal("fixture full root")
				}
				if scenario == "queue-full" {
					sequence.sendItems <- &TcpSendItem{}
				} else if scenario == "canceled" {
					sequence.Cancel()
				}
				accepted, err := send(&credit)
				want := scenario == "success" || scenario == "small-control-partition"
				if accepted != want || accepted && err != nil {
					t.Errorf("prepaid admission=%t err=%v want=%t", accepted, err, want)
				}
				if accepted {
					if credit != (natMemoryReservation{}) {
						t.Error("successful admission left duplicate caller credit")
					}
					if scenario == "success" {
						item := <-sequence.sendItems
						if item.memory != original {
							t.Error("final queue did not own the exact prepaid claim")
						}
						item.release()
					}
				} else {
					if credit != original {
						t.Error("failed final admission consumed caller credit")
					}
					credit.release()
					MessagePoolReturn(packet)
				}
				root.Release(fill)
				if root.UsedByteCount() != 0 || budget.UsedByteCount() != 0 || creditBudget.UsedByteCount() != 0 {
					t.Error("final/canceled admission leaked packet credit")
				}
				if scenario == "canceled" && sequence.ctx.Err() != context.Canceled {
					t.Error("canceled admission lost terminal context")
				}
			})
		}
	}
}
