// Non-feedback bridge groups retain their ordinary shared physical partition.
package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// DATA, mixed groups and every TCP lifecycle edge keep the shared two-frame
// partition and exactly one logical terminal after all real physical receipts.
func TestOrdinaryBridgeNonFeedbackPreservesSharedChunks(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, c := range []struct {
			name        string
			middleFlags byte
			payload     bool
			allData     bool
			immediate   bool
		}{
			{name: "data", middleFlags: tcpFlagAck, payload: true, allData: true},
			{name: "mixed", middleFlags: tcpFlagAck, payload: true},
			{name: "synack", middleFlags: tcpFlagAck | tcpFlagSyn, immediate: true},
			{name: "finack", middleFlags: tcpFlagAck | tcpFlagFin},
			{name: "rstack", middleFlags: tcpFlagAck | tcpFlagRst},
		} {
			var terminals atomic.Int64
			var terminalErrors atomic.Int64
			completionChunks := 0
			admittedMembers := 0
			f := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
				settings.SequenceBufferSize = DefaultClientSettings().SendBufferSettings.SequenceBufferSize
				settings.ProtocolVersion = DefaultProtocolVersion
				settings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
					if event.Phase == SendPackLifecyclePhaseTerminal {
						if event.Err != nil {
							terminalErrors.Add(1)
						}
						terminals.Add(1)
					}
				}
				settings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
					if group, ok := target.(*parsedPacketGroup); ok {
						admittedMembers += len(group.packets)
					}
				}
				settings.afterCreateSendGroupCompletionForTest = func(_ sendSequenceId, count int) { completionChunks = count }
			}, nil, TransportTypeP2p)
			parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
			parent.settings.TcpCollapsePrevention = true
			parent.settings.TcpCollapseMaxHold = 0
			selected := newPacketTransferTestChannel()
			selected.ctx, selected.client, selected.settings = parent.ctx, f.sender, parent.settings
			selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(f.receiver.ClientId())}
			update.client.Store(selected)
			t.Cleanup(closeParent)
			var received [][]byte
			f.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
				for _, frame := range frames {
					if !frame.Raw || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
						t.Errorf("%s changed raw provider frame shape", c.name)
						continue
					}
					received = append(received, bytes.Clone(frame.MessageBytes))
				}
			})
			templates := make([][]byte, 3)
			for index := range templates {
				flags := byte(tcpFlagAck)
				var payload []byte
				if index == 1 {
					flags = c.middleFlags
					if c.payload {
						payload = []byte{byte(index + 1)}
					}
				}
				if c.allData {
					payload = []byte{byte(index + 1)}
				}
				templates[index] = groupRecoveryPacket(1001+uint32(index), 5001+uint32(index), 4096, flags, payload)
			}
			packets := make([][]byte, len(templates))
			for index, packet := range templates {
				packets[index] = MessagePoolCopy(packet)
			}
			accepted := make([]bool, len(packets))
			before := time.Now()
			if count := parent.SendPacketBatchWithResults(SourceId(f.sender.ClientId()), protocol.ProvideMode_Network, packets, -1, accepted); count != len(packets) {
				t.Fatalf("%s ordinary batch refused: count=%d bits=%v", c.name, count, accepted)
			}
			for _, admitted := range accepted {
				if !admitted {
					t.Fatalf("%s member refused", c.name)
				}
			}
			synctest.Wait()
			var receipts []*windowRoundFrame
			for chunk, want := range []int{2, 1} {
				wire := f.take(f.senderOut)
				if wire.pack == nil || wire.pack.Nack || wire.pack.DeliverImmediately != c.immediate || len(wire.pack.Frames) != want || MessageByteCount(wire.pack.Frames) > DefaultMtu {
					t.Fatalf("%s changed shared chunk %d or reliable/lifecycle policy: pack=%+v", c.name, chunk, wire.pack)
				}
				f.forward(wire, f.receiverIn)
				for _, receipt := range f.takeAcknowledgements() {
					ack := receipt.ack
					if ack == nil || !bytes.Equal(ack.SequenceId, wire.pack.SequenceId) || !bytes.Equal(ack.MessageId, wire.pack.MessageId) || len(ack.MissingContractId) != 0 || len(ack.EvictedSequenceNumbers) != 0 {
						t.Fatalf("%s receipt did not name the real physical original", c.name)
					}
					receipts = append(receipts, receipt)
				}
			}
			synctest.Wait()
			source := f.sequence()
			if len(f.senderOut) != 0 || len(received) != len(templates) || completionChunks != 2 || admittedMembers != 3 || terminals.Load() != 0 || terminalErrors.Load() != 0 || !time.Now().Equal(before) {
				t.Fatalf("%s changed shared partition or logical completion: received=%d chunks=%d members=%d terminal=%d", c.name, len(received), completionChunks, admittedMembers, terminals.Load())
			}
			for index, packet := range templates {
				if !bytes.Equal(packet, received[index]) {
					t.Fatalf("%s changed native frame/event %d", c.name, index)
				}
			}
			for _, receipt := range receipts {
				f.forward(receipt, f.senderIn)
			}
			synctest.Wait()
			selected.stateLock.Lock()
			acked, pending := selected.packetStats.sendAckCount, selected.packetStats.sendNackCount
			selected.stateLock.Unlock()
			if count, size := source.resendQueue.QueueSize(); count != 0 || size != 0 || source.immediateSendCount != 0 || terminals.Load() != 1 || terminalErrors.Load() != 0 || acked != 3 || pending != 0 || !time.Now().Equal(before) {
				t.Fatalf("%s failed exact real receipt closure: retained=%d/%d immediate=%d terminal=%d errors=%d acked=%d pending=%d", c.name, count, size, source.immediateSendCount, terminals.Load(), terminalErrors.Load(), acked, pending)
			}
		}
	})
}
