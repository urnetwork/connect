package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Actual raw packet flags, including every member of a group, determine only
// the scheduling fence. Neither NoBudget nor the frame bytes are rewritten.
func TestProtocolSchedulingLifecycleKeepsSameFlowPredecessor(t *testing.T) {
	path := reliableDeliveryTestPath(4)
	key := ipSendSchedulingKey(path)
	for _, flags := range []byte{tcpFlagSyn, tcpFlagFin | tcpFlagAck, tcpFlagRst | tcpFlagAck} {
		for _, payload := range [][]byte{nil, {1}} {
			data := protocolOrderingPack(key, tcpFlagAck, []byte{1})
			control := protocolOrderingPack(key, flags, payload)
			control.NoBudget = true // Trusted policy does not grant reordering.
			laterAck := protocolOrderingPack(key, tcpFlagAck, nil)
			laterAck.NoBudget = true
			scheduler := newSendPackScheduler()
			scheduler.Push(data)
			scheduler.Push(control)
			scheduler.Push(laterAck)
			if !control.tcpOrderBarrier || scheduler.canPassTcpLifecycle(control) || scheduler.canPassTcpLifecycle(laterAck) {
				t.Fatalf("lifecycle or later ACK crossed same-flow DATA: flags=%x payload=%d", flags, len(payload))
			}
			if scheduler.TakeFifoEligible(func(*SendPack) bool { return true }) != data ||
				!scheduler.canPassTcpLifecycle(control) || scheduler.canPassTcpLifecycle(laterAck) {
				t.Fatal("removing DATA failed to preserve the next lifecycle boundary")
			}
		}
	}
}

func TestProtocolSchedulingAckAndUnrelatedFlowRemainEligible(t *testing.T) {
	key := ipSendSchedulingKey(reliableDeliveryTestPath(4))
	data := protocolOrderingPack(key, tcpFlagAck, []byte{1})
	ack := protocolOrderingPack(key, tcpFlagAck, nil)
	ack.NoBudget = true
	otherKey := key
	otherKey.ipFlow.sourcePort++
	other := protocolOrderingPack(otherKey, tcpFlagSyn, nil)
	other.NoBudget = true
	scheduler := newSendPackScheduler()
	for _, pack := range []*SendPack{data, ack, other} {
		scheduler.Push(pack)
	}
	eligible := func(pack *SendPack) bool { return pack.NoBudget && scheduler.canPassTcpLifecycle(pack) }
	if scheduler.TakeUnorderedEligible(eligible) != ack || scheduler.TakeUnorderedEligible(eligible) != other || scheduler.FifoHead() != data {
		t.Fatal("lifecycle isolation blocked pure ACK feedback or an unrelated flow")
	}
	mixed := protocolOrderingPack(key, tcpFlagAck, nil)
	mixed.Frames = append(mixed.Frames, protocolOrderingPack(key, tcpFlagFin|tcpFlagAck, nil).Frames...)
	if !mixed.hasTcpOrderBarrier() || !protocolOnlyFrames(mixed.Frames) {
		t.Fatal("a later pure FIN member lost the protocol group's lifecycle fence")
	}
}

func protocolOrderingPack(key sendSchedulingKey, flags byte, payload []byte) *SendPack {
	packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), flags, 100, payload)
	pack := &SendPack{
		Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}},
	}
	pack.schedulingKey = key
	pack.tcpOrderBarrier = pack.hasTcpOrderBarrier()
	return pack
}

// Zero configured queued slots remain a rendezvous. A real reliable ACK can
// still cross full DATA permission; its raw token is released before peer ACK.
func TestProtocolZeroBufferAckProgressAtFullDataBudget(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newProtocolDataPressureFixture(t, 4, false, func(settings *SendBufferSettings) {
			settings.SequenceBufferSize = 0
		})
		var result protocolPressureCompletion
		if admitted, err := fixture.send(t, 2, tcpFlagAck, 0, true, &result, time.Second); !admitted || err != nil {
			t.Fatalf("zero-buffer ACK blocked on DATA: admitted=%t error=%v", admitted, err)
		}
		wire := fixture.take(t, 2, 1)
		fixture.wire.sender.sendBuffer.mutex.Lock()
		sequence := fixture.wire.sender.sendBuffer.sendSequencesBySequenceId[RequireIdFromBytes(wire.pack.SequenceId)]
		fixture.wire.sender.sendBuffer.mutex.Unlock()
		synctest.Wait()
		sequence.protocolAdmission.mutex.Lock()
		count := sequence.protocolAdmission.count
		sequence.protocolAdmission.mutex.Unlock()
		if cap(sequence.packs) != 0 || cap(sequence.protocolPacks) != 0 || count != 0 || result.calls.Load() != 0 {
			t.Fatal("zero-buffer handoff gained a queue or held protocol count until ACK")
		}
		fixture.wire.forward(wire, fixture.wire.receiverIn)
		fixture.wire.acknowledge()
		if result.calls.Load() != 1 || result.success.Load() != 1 {
			t.Fatal("zero-buffer control lost its actual delivery ACK")
		}
		fixture.requireDataPressure(t)
		fixture.cancelData(t)
	})
}
