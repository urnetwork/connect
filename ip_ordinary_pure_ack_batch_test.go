// Ordinary bridge batches preserve every native feedback event inside bounded custody.
package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// One default-size bridge read must not exhaust physical feedback custody merely
// because its small, already-ready ACK frames were split into tiny originals.
func TestOrdinaryBridgePureAckBatchPreservesFeedbackCustody(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		defaults := DefaultClientSettings()
		limit := defaults.SendBufferSettings.SequenceBufferSize
		if limit != 32 || DefaultMultiClientSettings().PacketGroupMaxPacketCount != 64 {
			t.Fatal("fixture needs the actual default bridge-group and immediate bounds")
		}
		var starts, terminals, firstWrites, terminalErrors atomic.Int64
		var completionChunks int
		var admissionMembers []int
		f := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.SequenceBufferSize = limit
			settings.ProtocolVersion = DefaultProtocolVersion
			settings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				switch event.Phase {
				case SendPackLifecyclePhaseStarted:
					starts.Add(1)
				case SendPackLifecyclePhaseFirstRouteWrite:
					firstWrites.Add(1)
				case SendPackLifecyclePhaseTerminal:
					if event.Err != nil {
						terminalErrors.Add(1)
					}
					terminals.Add(1)
				}
			}
			settings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				if group, ok := target.(*parsedPacketGroup); ok {
					admissionMembers = append(admissionMembers, len(group.packets))
				}
			}
			settings.afterCreateSendGroupCompletionForTest = func(_ sendSequenceId, count int) {
				if completionChunks != 0 {
					t.Error("multiple completion owners for one bridge group")
				}
				completionChunks = count
			}
		}, nil, TransportTypeP2p)
		parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
		parent.settings.TcpCollapsePrevention = true
		parent.settings.TcpCollapseMaxHold = 0
		selected := newPacketTransferTestChannel()
		selected.ctx, selected.client, selected.settings = parent.ctx, f.sender, parent.settings
		selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(f.receiver.ClientId())}
		update.client.Store(selected)
		t.Cleanup(func() { closeParent(); selected.closeConnectSynOwners() })
		var received [][]byte
		f.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if !frame.Raw || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
					t.Error("ordinary producer changed its raw provider-IP frame shape")
					continue
				}
				received = append(received, bytes.Clone(frame.MessageBytes))
			}
		})
		offer := func(templates ...[]byte) {
			t.Helper()
			packets := make([][]byte, len(templates))
			accepted := make([]bool, len(templates))
			for index, packet := range templates {
				packets[index] = MessagePoolCopy(packet)
			}
			count := parent.SendPacketBatchWithResults(SourceId(f.sender.ClientId()),
				protocol.ProvideMode_Network, packets, -1, accepted)
			if count != len(templates) {
				t.Fatalf("ordinary bridge handoff: accepted=%d want=%d bits=%v", count, len(templates), accepted)
			}
			for _, ok := range accepted {
				if !ok {
					t.Fatal("ordinary per-input admission lost a group member")
				}
			}
		}
		prefix := groupRecoveryPacket(1001, 5001, 4096, tcpFlagAck, []byte("synthetic hdr"))
		offer(prefix)
		warm := f.take(f.senderOut)
		if warm.pack == nil || warm.pack.Nack || warm.pack.DeliverImmediately {
			t.Fatal("ordinary prefix lost reliable ordered policy")
		}
		f.forward(warm, f.receiverIn)
		f.acknowledge()
		source := f.sequence()
		if source.logicalLane != 0 || source.immediateSendLimit != limit || source.flowIsolation.Load() {
			t.Fatal("fixture changed default lane, custody or FIFO policy")
		}
		if count, size := source.resendQueue.QueueSize(); count != 0 || size != 0 || source.immediateSendCount != 0 || terminals.Load() != 1 {
			t.Fatal("the real initial prefix left stale reliable ownership")
		}
		const sequence = uint32(1014)
		templates := make([][]byte, 64)
		for index := range templates {
			templates[index] = groupRecoveryPacket(sequence, 6001+uint32(index), uint16(4096+index), tcpFlagAck, nil)
		}
		before := time.Now()
		offer(templates...)
		synctest.Wait()
		var receipts []*windowRoundFrame
		var originals int
		var encodedBytes int
		for len(f.senderOut) != 0 {
			wire := f.take(f.senderOut)
			if wire.pack == nil || wire.pack.Nack || !wire.pack.DeliverImmediately || len(wire.pack.Frames) == 0 || len(wire.pack.Frames) > sendPackH1GroupMaxFrames {
				t.Fatal("ordinary feedback chunk escaped reliable immediate storage bounds")
			}
			if MessageByteCount(wire.pack.Frames) > DefaultMtu {
				t.Fatal("feedback chunk exceeded the unchanged message-byte envelope")
			}
			encodedBytes += len(wire.bytes)
			originals++
			f.forward(wire, f.receiverIn)
			receipts = append(receipts, takePureAckCustodyReceipts(t, f, wire)...)
		}
		if len(received) != 65 || f.deliveredCount != 65 {
			t.Fatalf("first settled bridge group lost native feedback events: got=%d delivered=%d", len(received), f.deliveredCount)
		}
		if !bytes.Equal(received[0], prefix) {
			t.Fatal("initial TCP prefix changed")
		}
		for index, packet := range templates {
			if !bytes.Equal(received[index+1], packet) {
				t.Fatalf("ACK header/event %d changed or reordered", index)
			}
		}
		if completionChunks != originals || source.immediateSendCount != originals || originals > limit || terminals.Load() != 1 || !time.Now().Equal(before) {
			t.Fatalf("held physical receipt premise: chunks=%d originals=%d custody=%d terminal=%d elapsed=%s", completionChunks, originals, source.immediateSendCount, terminals.Load(), time.Since(before))
		}
		newer := groupRecoveryPacket(sequence, 7001, 8192, tcpFlagAck, nil)
		offer(newer)
		synctest.Wait()
		selected.stateLock.Lock()
		liveAcked, livePending := selected.packetStats.sendAckCount, selected.packetStats.sendNackCount
		selected.stateLock.Unlock()
		if source.ctx.Err() != nil || f.sender.ctx.Err() != nil || parent.ctx.Err() != nil ||
			starts.Load() != 3 || terminals.Load() != 1 || terminalErrors.Load() != 0 || liveAcked != 1 || livePending != 65 {
			t.Fatalf("newer feedback was not an independently live accepted owner: lifecycle=%d/%d errors=%d acked=%d pending=%d", starts.Load(), terminals.Load(), terminalErrors.Load(), liveAcked, livePending)
		}
		// Quiescence is the primary observation. No timer, lost ACK or native
		// retry is needed; every prior reliable wire receipt is still held.
		select {
		case encoded := <-f.senderOut:
			f.senderOut <- encoded
		default:
			source.protocolAdmission.mutex.Lock()
			pendingRaw := source.protocolAdmission.count
			source.protocolAdmission.mutex.Unlock()
			if pendingRaw != 1 || source.immediateSendCount != limit {
				t.Fatalf("missing feedback lacks the full-custody pending-owner premise: pending=%d custody=%d limit=%d", pendingRaw, source.immediateSendCount, limit)
			}
			t.Fatalf("PURE_TCP_ACK_BATCH_EXHAUSTED_CUSTODY: 64 accepted feedback frames consumed %d/%d physical owners and blocked the next accepted ACK before any receipt", originals, limit)
		}
		next := f.take(f.senderOut)
		if next.pack == nil || next.pack.Nack || !next.pack.DeliverImmediately || len(next.pack.Frames) != 1 || !bytes.Equal(next.pack.Frames[0].MessageBytes, newer) {
			t.Fatal("the later ordinary feedback owner changed its exact native bytes or reliability")
		}
		f.forward(next, f.receiverIn)
		receipts = append(receipts, takePureAckCustodyReceipts(t, f, next)...)
		if len(received) != 66 || !bytes.Equal(received[65], newer) || source.immediateSendCount != originals+1 || source.immediateSendCount > limit || terminals.Load() != 1 || !time.Now().Equal(before) {
			t.Fatal("fresh feedback spent an old receipt, changed time, or exceeded physical custody")
		}
		selected.stateLock.Lock()
		acked, pending := selected.packetStats.sendAckCount, selected.packetStats.sendNackCount
		selected.stateLock.Unlock()
		if acked != 1 || pending != 65 || len(admissionMembers) != 3 || admissionMembers[0] != 1 || admissionMembers[1] != 64 || admissionMembers[2] != 1 {
			t.Fatalf("logical bridge owners differ from physical chunk accounting: acked=%d pending=%d groups=%v", acked, pending, admissionMembers)
		}
		for _, receipt := range receipts {
			f.forward(receipt, f.senderIn)
		}
		synctest.Wait()
		selected.stateLock.Lock()
		acked, pending = selected.packetStats.sendAckCount, selected.packetStats.sendNackCount
		selected.stateLock.Unlock()
		if count, size := source.resendQueue.QueueSize(); count != 0 || size != 0 || source.immediateSendCount != 0 || acked != 66 || pending != 0 || starts.Load() != 3 || firstWrites.Load() != 3 || terminals.Load() != 3 || terminalErrors.Load() != 0 || !time.Now().Equal(before) {
			t.Fatalf("ordinary bridge callbacks/retention did not close exactly: count=%d bytes=%d immediate=%d acked=%d pending=%d lifecycle=%d/%d/%d", count, size, source.immediateSendCount, acked, pending, starts.Load(), firstWrites.Load(), terminals.Load())
		}
		t.Logf("ordinary ACK batch: inputs=64 physical_originals=%d encoded_bytes=%d unchanged_custody_limit=%d", originals, encodedBytes, limit)
	})
}
