package connect

import (
	"bytes"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The same provider can return valid application bytes without waiting for its
// missing reliable ordinal. The unanswered SYN clock belongs to ordered progress.
func TestNoAckDataAcrossTheReturnGapKeepsUnansweredConnect(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture := newAffinitySynQueueFixture(t, 4, nil)
		exit := fixture.first.selected
		fixture.raceOnly(exit)
		exit.clientReceivePacketCallback = fixture.parent.clientReceivePacket
		exit.reliabilitySettingsFunc = fixture.parent.ReliabilitySettings
		var delivered [][]byte
		fixture.parent.SetReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			delivered = append(delivered, bytes.Clone(packet))
		})

		clock := func() time.Time {
			t.Helper()
			stats, err := exit.WindowStats()
			if err != nil || !stats.connectProgressKnown {
				t.Fatalf("provider connect clock unavailable: %v", err)
			}
			return unansweredConnectSince(stats)
		}
		started := time.Now()
		update := fixture.initial(t)
		if !clock().Equal(started) {
			t.Fatal("the peer-confirmed opening did not retain its admission time")
		}
		returnWire := newWindowRoundFixtureWithIds(t, exit.args.Destination.Tail(), exit.ClientId(), nil, nil)
		wire := openImmediateParentHealthGap(t, exit, fixture.path, returnWire)
		returnSequence := returnWire.receiveSequence()
		ack := fixture.path.SequenceNumber + 1
		synAck := reraceTcpPacket(fixture.path.Reverse(), tcpFlagSyn|tcpFlagAck, 7000, ack, nil)
		immediate := wire.sendControl(synAck, true)
		if len(delivered) != 1 || !bytes.Equal(delivered[0], synAck) ||
			returnSequence.nextSequenceNumber != wire.base || !update.deferredInboundHealth || !clock().Equal(started) {
			t.Fatal("the initiating SYN-ACK did not reach the application ahead of the retained gap")
		}

		data := reraceTcpPacket(fixture.path.Reverse(), tcpFlagAck|tcpFlagPsh, 7001, ack, []byte("same return"))
		owned := MessagePoolCopy(data)
		frame, err := ipPacketFromProviderFrame(owned, 2)
		if err != nil {
			MessagePoolReturn(owned)
			t.Fatal(err)
		}
		admitted, err := returnWire.sender.SendWithTimeoutDetailed(frame, returnWire.receiver.ClientId(), nil, time.Second, NoAck())
		if !admitted {
			MessagePoolReturn(owned)
		}
		if !admitted || err != nil {
			t.Fatalf("real NoAck return refused: admitted=%t err=%v", admitted, err)
		}
		unreliable := returnWire.take(returnWire.senderOut)
		if unreliable.pack == nil || !unreliable.pack.Nack || unreliable.pack.DeliverImmediately ||
			!bytes.Equal(unreliable.pack.SequenceId, immediate.pack.SequenceId) {
			t.Fatal("the DATA did not use the same provider sequence in a NoAck Pack")
		}
		returnWire.forward(unreliable, returnWire.receiverIn)
		if len(delivered) != 2 || !bytes.Equal(delivered[1], data) || wire.missing.bytes == nil ||
			returnSequence.nextSequenceNumber != wire.base {
			t.Fatal("the NoAck DATA did not reach the application while the reliable ordinal remained missing")
		}
		if got := clock(); !got.Equal(started) {
			t.Errorf("NoAck DATA retired the unanswered SYN before reliable repair: clock=%v want=%v", got, started)
		}

		wire.repair()
		repair := ipOosTcpPacketSequence(wire.returnPath, tcpFlagAck|tcpFlagPsh, 1000, make([]byte, 1240))
		if len(delivered) != 3 || !bytes.Equal(delivered[2], repair) || returnSequence.nextSequenceNumber <= wire.base {
			t.Fatal("the retained original did not repair its own return sequence")
		}
		ordered := wire.sendControl(data, false)
		if ordered.pack.Nack || !bytes.Equal(ordered.pack.SequenceId, immediate.pack.SequenceId) ||
			ordered.pack.SequenceNumber <= immediate.pack.SequenceNumber ||
			len(delivered) != 4 || !bytes.Equal(delivered[3], data) {
			t.Fatal("the identical DATA did not reach the application on its repaired reliable sequence")
		}
		if got := clock(); !got.IsZero() {
			t.Errorf("same-sequence ordered DATA after repair retained the unanswered SYN: clock=%v", got)
		}
	})
}
