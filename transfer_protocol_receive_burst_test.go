package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Exempting protocol traffic must not reduce the established handoff capacity
// to one. Such a cold burst otherwise drops an original and waits for its RTO.
func TestProtocolReceiveColdBurstPreservesConfiguredHandoff(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, resume := make(chan struct{}), make(chan struct{})
		var enterOnce, resumeOnce sync.Once
		release := func() { resumeOnce.Do(func() { close(resume) }) }
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.ReceiveBufferSettings.SequenceBufferSize = 4
			settings.ReceiveBufferSettings.H1SequenceBufferSize = 4
			settings.ReceiveBufferSettings.H1SequenceBufferAdaptiveMaxSize = 0
			settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
				enterOnce.Do(func() { close(entered) })
				<-resume
			}
		})
		t.Cleanup(release)
		var completed atomic.Int32
		var firstSequence []byte
		beforeDrops := fixture.receiver.ReceiveStats().PackHandoffDropCount
		for number := range 2 {
			packet := MessagePoolCopy(ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 7000, nil))
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}
			if admitted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
				if err == nil {
					completed.Add(1)
				}
			}, time.Second, NoBudget()); !admitted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("public reliable protocol offer failed: %t %v", admitted, err)
			}
			wire := fixture.take(fixture.senderOut)
			if wire.pack == nil || wire.pack.Nack || !protocolOnlyPack(wire.pack) || wire.pack.SequenceNumber != uint64(number) {
				t.Fatal("public protocol burst lost its distinct reliable sequence identity")
			}
			if number == 0 {
				firstSequence = bytes.Clone(wire.pack.SequenceId)
			} else if !bytes.Equal(firstSequence, wire.pack.SequenceId) {
				t.Fatal("burst did not address the same real receiver sequence")
			}
			fixture.forward(wire, fixture.receiverIn)
			if number == 0 {
				<-entered
			}
		}
		sequence := fixture.receiveSequence()
		if completed.Load() != 0 || fixture.deliveredCount != 0 {
			t.Fatal("paused receiver published delivery or ACK without downstream ownership")
		}
		if drops := fixture.receiver.ReceiveStats().PackHandoffDropCount - beforeDrops; drops != 0 || len(sequence.protocolPacks) != 2 {
			t.Fatalf("protocol burst within configured count4 dropped at cold handoff: drops=%d queued=%d capacity=%d", drops, len(sequence.protocolPacks), cap(sequence.protocolPacks))
		}
		release()
		synctest.Wait()
		fixture.acknowledge()
		synctest.Wait()
		if fixture.deliveredCount != 2 || completed.Load() != 2 {
			t.Fatal("healthy protocol burst required retransmit time before delivery/ACK")
		}
	})
}
