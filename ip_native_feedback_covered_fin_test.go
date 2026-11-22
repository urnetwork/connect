// A peer receipt for later native data can explain an older, unwritten replay
// across a local half-close. A local FIN alone provides no such receipt.
package connect

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// An earlier native replay is still waiting for real immediate custody when the
// peer accepts cumulative feedback in a later, ordered one-byte data packet.
func TestNativeFeedbackReplayCoveredByPeerAckedDataBeforeLocalFin(t *testing.T) {
	testNativeFeedbackReplayBeforeLocalFin(t, 502, true, false)
}

// The same local half-close lacks evidence when no covering data was written.
func TestNativeFeedbackReplayWithoutCoveringDataBeforeLocalFinStaysStrict(t *testing.T) {
	testNativeFeedbackReplayBeforeLocalFin(t, 0, false, false)
}

// Physical serialization of the later data does not supply its peer receipt.
func TestNativeFeedbackReplayUnacknowledgedDataBeforeLocalFinStaysStrict(t *testing.T) {
	testNativeFeedbackReplayBeforeLocalFin(t, 502, false, false)
}

// An independently delivered data member with the old cumulative ACK does
// not prove delivery of a strictly advancing replacement.
func TestNativeFeedbackReplayEqualAckDataBeforeLocalFinStaysStrict(t *testing.T) {
	testNativeFeedbackReplayBeforeLocalFin(t, 501, true, false)
}

// An earlier independently acknowledged data packet cannot certify a later
// original, even on the same live TCP tuple, source sequence and selection.
func TestNativeFeedbackReplayPeerDataBeforeRawRefusalStaysStrict(t *testing.T) {
	testNativeFeedbackReplayBeforeLocalFin(t, 0, false, true)
}

// Two held immediate items let ordinary data pass while the admitted replay
// waits. The later FIN enters the scheduler but keeps its source order on wire.
func testNativeFeedbackReplayBeforeLocalFin(t *testing.T, dataAck uint32, confirmData, historicalData bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const limit = 2
		var source atomic.Pointer[SendSequence]
		groups := make(chan *parsedPacketGroup, 8)
		stages := make(chan SendPackLifecycleObservation, 24)
		f := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = limit
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(sequence *SendSequence) {
				source.Store(sequence)
			}
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				groups <- target.(*parsedPacketGroup)
			}
			previous := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				previous(event)
				if event.MessageType == protocol.MessageType_IpIpPacketToProvider {
					stages <- event
				}
			}
		})
		t.Cleanup(func() {
			f.update.Close()
			f.parent.discardNativeAckRetries()
		})

		const sourceSequence, base = 100, 500
		zero := groupDispositionControlPacket(sourceSequence, base, 0, tcpFlagAck)
		older := groupDispositionControlPacket(sourceSequence, base+1, 4096, tcpFlagAck)
		parsed := groupRecoveryParsed(t, zero)
		f.update.ipPath = parsed.ipPath
		f.update.updateSequence(parsed)
		f.update.receivedInbound.Store(true)
		f.parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{parsed.ipPath.ToIp4Path(): f.update}
		f.parent.flowReaperDone = make(chan struct{})
		f.parent.flowReaperWake = make(chan struct{}, 1)

		offer := func(packet []byte, want bool) {
			t.Helper()
			original := MessagePoolCopy(packet)
			f.witnesses = append(f.witnesses, MessagePoolShareReadOnly(original))
			accepted := [1]bool{!want}
			count := f.parent.SendNativePacketBatchWithResults(f.source, protocol.ProvideMode_Network, [][]byte{original}, 0, accepted[:])
			if accepted[0] != want || (count == 1) != want || count < 0 || count > 1 {
				t.Fatalf("native admission count=%d accepted=%t want=%t", count, accepted[0], want)
			}
		}
		wire := func(packet []byte, immediate bool) *protocol.Pack {
			t.Helper()
			synctest.Wait()
			select {
			case encoded := <-f.route:
				defer MessagePoolReturn(encoded)
				pack := decodeSendPackLifecycleWirePack(t, encoded)
				if pack.Nack || pack.DeliverImmediately != immediate || len(pack.Frames) != 1 ||
					!bytes.Equal(pack.Frames[0].MessageBytes, packet) {
					t.Fatal("physical write did not carry the exact reliable native packet and delivery class")
				}
				return pack
			default:
				t.Fatal("expected native packet did not reach the physical route")
				return nil
			}
		}
		var sequence *SendSequence
		var last *protocol.Pack
		var attempt *tcpNativeAckRetryAttempt
		var replay SendPackLifecycleObservation
		startReplay := func(first bool) {
			t.Helper()
			offer(zero, true)
			if first {
				f.requireParked()
				f.unpark()
			}
			wire(zero, true)
			offer(zero, true)
			last = wire(zero, true)
			sequence = source.Load()
			if sequence == nil || sequence.immediateSendCount != limit || sequence.immediateSendLimit != limit {
				t.Fatal("held peer receipts did not exhaust actual immediate custody")
			}
			for len(groups) != 0 {
				<-groups
			}

			// Occupy only raw admission while the physical predecessors stay unacknowledged.
			admission := sequence.protocolAdmission
			reserved := 0
			release := func() {
				for reserved > 0 {
					admission.release(sendSchedulingKey{})
					reserved--
				}
			}
			t.Cleanup(release)
			for range limit {
				acquired, closed, _ := admission.tryAcquire(sendSchedulingKey{}, false)
				if !acquired || closed {
					t.Fatal("could not occupy actual protocol admission after its physical originals were serialized")
				}
				reserved++
			}
			offer(older, false)
			owner := f.update.nativeAckRetry
			if owner == nil || owner.attempt.source != sequence || owner.credit == nil || !bytes.Equal(owner.packet, older) {
				t.Fatal("the original refusal did not retain the exact advancing feedback")
			}
			release()
			if !owner.attempt.gate.watchGranted(&owner.waiter, owner.installation) {
				t.Fatal("freeing the raw slots did not grant the existing replay")
			}
			for len(stages) != 0 {
				<-stages
			}
			f.parent.drainNativeAckRetries()
			synctest.Wait()
			if len(groups) != 1 || len(stages) != 1 || len(f.route) != 0 {
				t.Fatalf("expected one admitted replay waiting solely for immediate custody: groups=%d phases=%d wire=%d", len(groups), len(stages), len(f.route))
			}
			replayGroup := <-groups
			replay = <-stages
			attempt = replayGroup.nativeAckRetry
			if attempt == nil || !attempt.replay || attempt.source != sequence || replayGroup.collapseSource != sequence ||
				replayGroup.completionFlags.Load()&(groupCompletionAdmitted|groupCompletionDequeued) != groupCompletionAdmitted|groupCompletionDequeued ||
				replay.Phase != SendPackLifecyclePhaseStarted || replay.ClientId != f.client.ClientId() ||
				replay.DestinationId != f.peer || replay.Token == 0 || !replay.AckRequired ||
				f.update.nativeAckRetry != nil || owner.credit != nil || owner.packet != nil {
				t.Fatal("the granted replay did not reach this source queue with its original custody returned")
			}

		}
		startReplay(true)
		if historicalData {
			// Complete data with a higher ACK before the second real raw refusal.
			// A later duplicate lower ACK can reach this same native source.
			packet := groupRecoveryPacket(sourceSequence, base+2, 4096, tcpFlagAck, []byte{1})
			offer(packet, true)
			prior := wire(packet, false)
			data := <-groups
			acknowledgeSendPackLifecycleWirePack(t, f.client, f.peer, prior)
			synctest.Wait()
			if data.completionFlags.Load()&groupCompletionTerminal == 0 || sequence.immediateSendCount != 0 ||
				sequence.resendQueue.Len() != 0 || len(f.route) != 0 {
				t.Fatal("the historical data and its original source did not settle before a second raw refusal")
			}
			startReplay(false)
		}

		var completion *parsedPacketGroup
		finSequence := uint32(sourceSequence)
		if historicalData {
			finSequence++
		}
		if dataAck != 0 {
			packet := groupRecoveryPacket(sourceSequence, dataAck, 4096, tcpFlagAck, []byte{1})
			offer(packet, true)
			dataPack := wire(packet, false)
			if confirmData {
				last = dataPack
			}
			completion = <-groups
			if completion.collapseSource != sequence || completion.completionFlags.Load()&groupCompletionTerminal != 0 {
				t.Fatal("the actual covering data lacks its independent outstanding peer receipt")
			}
			finSequence++
		}
		fin := groupDispositionControlPacket(finSequence, base+2, 4096, tcpFlagAck|tcpFlagFin)
		offer(fin, true)
		synctest.Wait()
		if len(groups) != 1 || len(f.route) != 0 || sequence.immediateSendCount != limit {
			t.Fatal("queued FIN crossed the held replay or released its predecessor")
		}
		finGroup := <-groups
		f.update.stateLock.Lock()
		halfClosed := finGroup.completionFlags.Load()&groupCompletionDequeued != 0 &&
			f.update.egressFinSeen && !f.update.ingressFinSeen && f.update.egressFinSequence == finSequence+1 &&
			f.update.egressAckSequence == base+2 && f.update.nativeAckLastUnrelatedSerial > attempt.serial &&
			f.update.sequenceAdmissionEpoch == attempt.epoch && f.update.client.loadSelection() == attempt.selection &&
			!f.update.IsDone() && !f.update.sourceReset.Load() && !f.update.pendingCollapseCloseWithLock()
		f.update.stateLock.Unlock()
		if !halfClosed {
			t.Fatal("local FIN did not leave the exact live sender generation half-closed at source dequeue")
		}

		// A cumulative receipt synchronously completes the named real item before
		// its next raw group. In the unconfirmed case the receipt names the last prior ACK.
		acknowledgeSendPackLifecycleWirePack(t, f.client, f.peer, last)
		synctest.Wait()
		if completion != nil && (completion.completionFlags.Load()&groupCompletionTerminal != 0) != confirmData {
			t.Fatalf("the actual data terminal did not match its independent peer receipt: confirmed=%t", confirmData)
		}
		f.acknowledgeAll(fin)
		if sequence.immediateSendCount != 0 || sequence.resendQueue.Len() != 0 {
			t.Fatal("peer acknowledgements did not retire all actual wire owners")
		}
		var first, terminal error
		phases := 0
		for len(stages) != 0 {
			event := <-stages
			if event.Token != replay.Token {
				continue
			}
			if event.ClientId != replay.ClientId || event.DestinationId != replay.DestinationId || !event.AckRequired {
				t.Fatal("replay changed client, peer or reliability across its phases")
			}
			phases++
			switch event.Phase {
			case SendPackLifecyclePhaseFirstRouteWrite:
				first = event.Err
			case SendPackLifecyclePhaseTerminal:
				terminal = event.Err
			}
		}
		if phases != 2 || !errors.Is(first, errNativeAckRetrySuperseded) || !errors.Is(terminal, errNativeAckRetrySuperseded) ||
			!sendPackFailedUnwritten(first) || !sendPackFailedUnwritten(terminal) {
			t.Fatalf("the admitted replay did not report both unwritten phases: phases=%d first=%v terminal=%v", phases, first, terminal)
		}
		coveringData := dataAck > base+1 && confirmData
		if IsSendPackNativeAckRetrySuperseded(first) != coveringData || IsSendPackNativeAckRetrySuperseded(terminal) != coveringData {
			t.Errorf("native replay lost the distinction between independently peer-acknowledged covering DATA and a lone local FIN: covering_data=%t first=%v terminal=%v", coveringData, first, terminal)
		}
	})
}
