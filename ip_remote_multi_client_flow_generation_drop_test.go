// A packet whose exact tuple can take no flow generation is refused, and the
// refusal is counted in FlowGenerationDropCount, never left unexplained. The
// roots drive the real multi client, its exits and its delivery callback;
// channels and synctest order the interleavings, never elapsed time.
package connect

import (
	"sync"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// Opens path on exit a and closes it from both ends, the server first, until
// the multi client retires the completed generation. Returns the app's next
// sequence number, the server's, and the server's final ACK.
func flowGenerationDropCompletedFlow(t *testing.T, harness *reraceHarness, path *IpPath) (uint32, uint32, []byte) {
	t.Helper()
	if _, _, ok := harness.connect(path); !ok {
		t.Fatal("fixture flow did not connect")
	}
	appSequence := path.SequenceNumber + 1
	serverSequence := reraceServerIsn + 1
	harness.inject("a", reraceTcpPacket(path.Reverse(), tcpFlagFin|tcpFlagAck, serverSequence, appSequence, nil))
	if !harness.send(reraceTcpPacket(path, tcpFlagFin|tcpFlagAck, appSequence, serverSequence+1, nil)) {
		t.Fatal("the app's FIN was refused")
	}
	finalAck := reraceTcpPacket(path.Reverse(), tcpFlagAck, serverSequence+1, appSequence+1, nil)
	harness.inject("a", finalAck)
	if update := harness.update(path); update != nil && !update.IsDone() {
		t.Fatal("the closed flow's generation did not end")
	}
	return appSequence + 1, serverSequence + 1, finalAck
}

// The refusal the sdk soak found unexplained once in 106 tip-check runs
// (TestDeviceLocalSyntheticDeviceRemoteMemorySoak): the provider repeats its
// final ACK to a completed flow, the device's TCP answers the repeat with a
// reset before that delivery callback returns, and the reset finds its tuple
// held by the ended generation's executing callback. Before FlowGenerationDropCount
// the refusal was counted nowhere: neither a block nor a collapse.
func TestFlowGenerationDropCountsAnswerDuringCompletedFlowDelivery(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{{name: "a"}}, nil)
		path := reraceFlow(4, 0, 51410)
		appSequence, _, finalAck := flowGenerationDropCompletedFlow(t, harness, path)

		entered, release := make(chan struct{}), make(chan struct{})
		var holdOnce sync.Once
		harness.multiClient.SetReceivePacketCallback(func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
			harness.receive(source, provideMode, ipPath, packet)
			holdOnce.Do(func() {
				close(entered)
				<-release
			})
		})
		harness.exit("a").sendTo(harness.channel("a").ClientId(), finalAck)
		synctest.Wait()
		select {
		case <-entered:
		default:
			close(release)
			t.Fatal("the repeated final ACK was not delivered to the app")
		}

		blocked := harness.multiClient.PacketStats().BlockEgressPacketCount
		collapsed := harness.multiClient.TcpCollapseDropCount()
		reset := MessagePoolCopy(reraceTcpPacket(path, tcpFlagRst, appSequence, 0, nil))
		accepted := harness.multiClient.SendNativePacketBatch(harness.source, protocol.ProvideMode_Network, [][]byte{reset}, 0)
		dropped := harness.multiClient.FlowGenerationDropCount()
		close(release)
		harness.settle()
		if accepted != 0 {
			t.Fatalf("the reset was taken while the ended generation's callback ran: accepted=%d", accepted)
		}
		if dropped != 1 {
			t.Fatalf("the refused reset was not counted as a flow generation drop: count=%d", dropped)
		}
		if harness.multiClient.PacketStats().BlockEgressPacketCount != blocked || harness.multiClient.TcpCollapseDropCount() != collapsed {
			t.Fatal("the generation refusal was also counted as a block or a collapse")
		}

		// once the callback returns, the tuple takes a generation again
		reset = MessagePoolCopy(reraceTcpPacket(path, tcpFlagRst, appSequence, 0, nil))
		harness.multiClient.SendNativePacketBatch(harness.source, protocol.ProvideMode_Network, [][]byte{reset}, 0)
		harness.settle()
		if got := harness.multiClient.FlowGenerationDropCount(); got != 1 {
			t.Fatalf("a send after the callback returned was refused for its generation: count=%d", got)
		}
	})
}

// A closed client takes no new generation, and its refusals are counted the
// same way, so a packet refused at teardown still names its reason.
func TestFlowGenerationDropCountsClosedClientRefusal(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{{name: "a"}}, nil)
		path := reraceFlow(4, 0, 51411)
		harness.multiClient.Close()
		harness.settle()
		syn := MessagePoolCopy(reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil))
		accepted := harness.multiClient.SendNativePacketBatch(harness.source, protocol.ProvideMode_Network, [][]byte{syn}, 0)
		if accepted != 0 {
			t.Fatalf("a closed client took a packet: accepted=%d", accepted)
		}
		if got := harness.multiClient.FlowGenerationDropCount(); got != 1 {
			t.Fatalf("the closed client's refusal was not counted: count=%d", got)
		}
	})
}

// A SYN whose generation ends between its lookup and its collapse admission
// is refused there, and counted once.
func TestFlowGenerationDropCountsSynWhoseGenerationEndsBeforeAdmission(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{{name: "a"}}, nil)
		path := reraceFlow(4, 0, 51412)
		ended := 0
		harness.multiClient.beforePacketGroupGateForTest = func(update *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if reraceSameFlow(group.ipPath, path) {
				ended++
				update.cancel()
			}
		}
		syn := MessagePoolCopy(reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil))
		accepted := harness.multiClient.SendNativePacketBatch(harness.source, protocol.ProvideMode_Network, [][]byte{syn}, 0)
		harness.multiClient.beforePacketGroupGateForTest = nil
		harness.settle()
		if ended != 1 {
			t.Fatalf("fixture did not end the generation inside the offer: ended=%d", ended)
		}
		if accepted != 0 {
			t.Fatalf("a SYN for an ended generation was taken: accepted=%d", accepted)
		}
		if got := harness.multiClient.FlowGenerationDropCount(); got != 1 {
			t.Fatalf("the SYN refused for its ended generation was not counted once: count=%d", got)
		}
		if harness.multiClient.TcpCollapseDropCount() != 0 || harness.multiClient.PacketStats().BlockEgressPacketCount != 0 {
			t.Fatal("the generation refusal was also counted as a collapse or a block")
		}
	})
}
