// A provider return stream wedged behind a missing DATA Pack must not keep
// handing a client SYN-ACKs and pure ACKs ahead of that gap once it is older
// than two seconds. Real provider, real signed Clients, controlled wire.
package connect

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// What the client observed for the control original sent behind the gap.
type providerControlGapObservation struct {
	wireImmediate bool
	// control packets the client application saw while the gap was open
	beforeRepair int32
	// and after the missing DATA finally arrived
	afterRepair int32
	want        int32
}

// Runs the wedged-return shape through the public provider return API: one
// DATA Pack is lost on every write, a later DATA Pack waits behind it at the
// client, the gap ages by gapAge, and a new flow's SYN-ACK (with a pure ACK in
// the batch form) is offered. The fake clock only moves inside time.Sleep.
func observeProviderControlBehindGap(t *testing.T, batch bool, gapAge time.Duration) providerControlGapObservation {
	t.Helper()
	var observation providerControlGapObservation
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(kib(512))
		tx := NewTransferMemoryBudgetWithParent(kib(128), root)
		rx := NewTransferMemoryBudgetWithParent(kib(128), root)
		head := NewTransferMemoryBudgetWithParent(kib(128), root)
		pack := NewTransferMemoryBudgetWithParent(kib(128), root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{tx, rx, head, pack, root} {
				assertRetainedBudgetBalance(t, budget)
			}
		})
		fixture, _, _, _ := newConsumerPaidPackByteFixture(t,
			consumerPaidPackByteLayout{label: "provider-control-gap-age", byteLimit: kib(128), leafBytes: kib(128), rootBytes: kib(512), countLimit: 32},
			rx, head, pack, nil, func(receiver bool, settings *ClientSettings) {
				if !receiver {
					settings.SendBufferSettings.SequenceBufferSize = 32
					settings.SendBufferSettings.ResendQueueBudget = tx
					settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
					settings.SendBufferSettings.ResendQueueMinByteCount = 0
				}
			})
		nat := NewLocalUserNatWithDefaults(fixture.ctx, "control-gap-age-test")
		settings := DefaultRemoteUserNatProviderSettings()
		settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		settings.ProtocolVersion = 2
		provider := NewRemoteUserNatProvider(fixture.sender, nat, settings)
		if provider == nil || nat == nil {
			t.Fatal("actual provider construction refused")
		}
		t.Cleanup(func() {
			provider.Close()
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := nat.CloseAndWait(closeCtx); err != nil {
				t.Error(err)
			}
		})
		path := reliableDeliveryTestPath(4)
		controlPath := *path
		controlPath.SourcePort++ // a new TCP flow on the same exit
		warmPacket := ipOosTcpPacketSequence(path, tcpFlagAck, 1000, []byte("warm"))
		missingPacket := ipOosTcpPacketSequence(path, tcpFlagAck, 1004, bytes.Repeat([]byte{0x58}, 1240))
		laterPacket := ipOosTcpPacketSequence(path, tcpFlagAck, 2244, bytes.Repeat([]byte{0x59}, 1240))
		synAckPacket := ipOosTcpPacketSequence(&controlPath, tcpFlagSyn|tcpFlagAck, 7000, nil)
		ackPacket := ipOosTcpPacketSequence(&controlPath, tcpFlagAck, 7001, nil)
		var controlVisits atomic.Int32
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				actual, err := ipPacketFromProviderBytes(frame)
				if err == nil && (bytes.Equal(actual, synAckPacket) || bytes.Equal(actual, ackPacket)) {
					controlVisits.Add(1)
				}
			}
		})
		offer := func(packetPath *IpPath, packet []byte) *windowRoundFrame {
			provider.Receive(SourceId(fixture.receiver.ClientId()), protocol.ProvideMode_Network, packetPath, packet)
			return synAckProviderApplicationWire(t, fixture, packet)
		}

		warm := offer(path, warmPacket)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		missing := offer(path, missingPacket)
		later := offer(path, laterPacket)
		fixture.forward(later, fixture.receiverIn)
		fixture.acknowledge()
		receiver := fixture.receiveSequence()
		base := missing.pack.SequenceNumber
		if later.pack.SequenceNumber != base+1 || receiver.nextSequenceNumber != base || receiver.receiveQueue.Len() != 1 {
			t.Fatal("later DATA is not waiting at the client behind the missing DATA")
		}

		started := time.Now()
		time.Sleep(gapAge)
		synctest.Wait()
		// The wedge: every recovery copy of the missing DATA is lost too.
		// Contract control traffic still reaches the client.
		for len(fixture.senderOut) != 0 {
			retry := fixture.take(fixture.senderOut)
			if retry.pack != nil && len(retry.pack.Frames) == 0 {
				fixture.forward(retry, fixture.receiverIn)
				continue
			}
			if retry.pack == nil || retry.pack.SequenceNumber != base ||
				!bytes.Equal(retry.pack.SequenceId, missing.pack.SequenceId) {
				t.Fatal("the aged gap produced provider traffic other than recovery of the missing DATA")
			}
			fixture.drop(retry)
		}
		fixture.acknowledge()
		if time.Since(started) != gapAge || receiver.nextSequenceNumber != base {
			t.Fatal("the gap did not stay open for exactly the requested age")
		}

		var control *windowRoundFrame
		observation.want = 1
		if batch {
			provider.ReceiveBatch(SourceId(fixture.receiver.ClientId()), protocol.ProvideMode_Network, &controlPath, [][]byte{synAckPacket, ackPacket})
			control = synAckProviderApplicationWire(t, fixture, synAckPacket, ackPacket)
			observation.want = 2
		} else {
			control = offer(&controlPath, synAckPacket)
		}
		if control.pack.SequenceNumber != base+2 || control.pack.Head || control.pack.Nack ||
			!bytes.Equal(control.pack.SequenceId, missing.pack.SequenceId) {
			t.Fatal("the SYN-ACK did not follow the waiting DATA in the same return generation")
		}
		observation.wireImmediate = control.pack.DeliverImmediately
		fixture.forward(control, fixture.receiverIn)
		fixture.acknowledge()
		observation.beforeRepair = controlVisits.Load()

		// The missing DATA finally arrives: everything drains in order.
		fixture.forward(missing, fixture.receiverIn)
		fixture.acknowledge()
		observation.afterRepair = controlVisits.Load()
		if receiver.nextSequenceNumber != base+3 {
			t.Fatalf("the repaired return stream did not drain through the control original: floor=%d want=%d",
				receiver.nextSequenceNumber, base+3)
		}
		sequence := fixture.sequence()
		if len(sequence.sendItems) != 0 || sequence.immediateSendCount != 0 {
			t.Fatal("the repaired return stream left a provider original or immediate count outstanding")
		}
		t.Logf("PROVIDER_CONTROL_GAP batch=%t gap_age=%s wire_immediate=%t before_repair=%d after_repair=%d",
			batch, gapAge, observation.wireImmediate, observation.beforeRepair, observation.afterRepair)
	})
	return observation
}

// Root cause: a provider whose return stream is wedged behind a gap older
// than two seconds still sent a new flow's SYN-ACK for immediate delivery, so
// the client completed a handshake through a stuck exit while that flow's
// data would wait behind the gap until GapTimeout.
func TestProviderSynAckBehindGapOlderThanTwoSecondsWaitsForRepair(t *testing.T) {
	observation := observeProviderControlBehindGap(t, false, 2*time.Second+time.Millisecond)
	if observation.wireImmediate || observation.beforeRepair != 0 {
		t.Errorf("SYNACK_AHEAD_OF_AGED_GAP: a provider wedged behind a 2.001s gap still delivered its SYN-ACK ahead of the gap: wire_immediate=%t delivered_before_repair=%d",
			observation.wireImmediate, observation.beforeRepair)
	}
	if observation.afterRepair != observation.want {
		t.Errorf("the SYN-ACK was not delivered in order once the gap repaired: delivered=%d want=%d",
			observation.afterRepair, observation.want)
	}
}

// Root cause, batch form: the pure ACK rides the same policy as the SYN-ACK.
func TestProviderSynAckAndAckBatchBehindGapOlderThanTwoSecondsWaitsForRepair(t *testing.T) {
	observation := observeProviderControlBehindGap(t, true, 2*time.Second+time.Millisecond)
	if observation.wireImmediate || observation.beforeRepair != 0 {
		t.Errorf("ACK_AHEAD_OF_AGED_GAP: a provider wedged behind a 2.001s gap still delivered a SYN-ACK and pure ACK ahead of the gap: wire_immediate=%t delivered_before_repair=%d",
			observation.wireImmediate, observation.beforeRepair)
	}
	if observation.afterRepair != observation.want {
		t.Errorf("the controls were not delivered in order once the gap repaired: delivered=%d want=%d",
			observation.afterRepair, observation.want)
	}
}

// Guard: a gap at most two seconds old keeps ba795437's immediate delivery.
// Two seconds exactly is still young.
func TestGuardProviderSynAckBehindTwoSecondGapStaysImmediate(t *testing.T) {
	for _, batch := range []bool{false, true} {
		observation := observeProviderControlBehindGap(t, batch, 2*time.Second)
		if !observation.wireImmediate || observation.beforeRepair != observation.want || observation.afterRepair != observation.want {
			t.Errorf("guard: a 2s gap lost immediate delivery: batch=%t wire_immediate=%t delivered_before_repair=%d after_repair=%d want=%d",
				batch, observation.wireImmediate, observation.beforeRepair, observation.afterRepair, observation.want)
		}
	}
}
