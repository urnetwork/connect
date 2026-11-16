// Covered upload data must not suppress independently useful duplicate acks
// sharing a native read burst. Both roots keep the real Transfer/provider path.
package connect

import (
	"bytes"
	"io"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The indexed native entry must refuse the covered data member while its three
// pure acknowledgements repair an actual post-Transfer loss before any timer.
func TestNativeMixedCoveredDataPreservesDuplicateAckFastRecovery(t *testing.T) {
	checkNativeMixedCoveredDataAckRecovery(t, false)
}

// The concrete UpgradeMux native entry must preserve the same partial outcome
// through its own grouper and direct native-group upstream capability.
func TestUpgradeMuxMixedCoveredDataPreservesDuplicateAckFastRecovery(t *testing.T) {
	checkNativeMixedCoveredDataAckRecovery(t, true)
}

// Uses the unchanged actual-native fixture. A passive mux wrapper creates no
// local resolver, server or tun worker; the concrete native entry, generic mux
// grouping, Multi policy/collapse, Transfer and provider dispatch all run.
func checkNativeMixedCoveredDataAckRecovery(t *testing.T, throughUpgradeMux bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeDuplicateAckFixture(t, true)
		var ownedPackets, witnesses [][]byte
		uploadDone := make(chan struct{})
		uploadStarted := false
		defer func() {
			fixture.close()
			if uploadStarted {
				<-uploadDone
			}
			requireGroupTestWitnessesReleased(t, ownedPackets, witnesses)
		}()

		// Keep the caller's original references observable until joined cleanup,
		// including early fatal paths after a consuming native entry returns.
		copyPackets := func(templates ...[]byte) [][]byte {
			packets := make([][]byte, len(templates))
			for index, template := range templates {
				packets[index] = MessagePoolCopy(template)
			}
			packetWitnesses := groupTestPacketWitnesses(t, packets)
			ownedPackets = append(ownedPackets, packets...)
			witnesses = append(witnesses, packetWitnesses...)
			return packets
		}
		var mux *UpgradeMux
		entry := "native-indexed"
		if throughUpgradeMux {
			mux = &UpgradeMux{mux: &IpMux{}}
			mux.SetUpstreamBatchClient(fixture.parent)
			entry = "upgrade-mux-native"
		}
		// Both entries take every packet, including each refused member.
		offer := func(packets [][]byte, accepted []bool) int {
			source := SourceId(fixture.sender.ClientId())
			if throughUpgradeMux {
				return mux.SendNativePacketBatch(source, fixture.parent.provideMode, packets, 5*time.Second)
			}
			return fixture.parent.SendNativePacketBatchWithResults(source, fixture.parent.provideMode,
				packets, 5*time.Second, accepted)
		}

		fixture.consumer.stateLock.Lock()
		base := fixture.consumer.base
		fixture.consumer.stateLock.Unlock()
		upload := []byte("data")
		uploadEnd := fixture.initial + 1 + uint32(len(upload))
		data := groupRecoveryPacket(fixture.initial+1, base, fixture.window, tcpFlagAck, upload)
		type uploadResult struct {
			payload []byte
			err     error
		}
		uploaded := make(chan uploadResult, 1)
		uploadStarted = true
		go func() {
			defer close(uploadDone)
			payload := make([]byte, len(upload))
			_, err := io.ReadFull(fixture.origin, payload)
			uploaded <- uploadResult{payload: payload, err: err}
		}()
		if !fixture.offer(data) {
			t.Fatal("initial native upload DATA was refused")
		}
		synctest.Wait()
		select {
		case result := <-uploaded:
			if result.err != nil || !bytes.Equal(result.payload, upload) {
				t.Fatalf("actual provider upload: payload=%q err=%v", result.payload, result.err)
			}
		default:
			t.Fatal("initial upload did not reach the actual origin at quiescence")
		}

		// The provider's inner acknowledgement is the duplicate's exclusive
		// right edge, so this upload cannot use its recovery lease for collapse.
		dataPath, dataPayload, err := ParseIpPathWithPayload(data)
		if err != nil {
			t.Fatalf("covered upload template: %v", err)
		}
		duplicate := parsedPacket{packet: data, ipPath: dataPath, payload: dataPayload}
		fixture.update.stateLock.Lock()
		covered := fixture.update.sequenceCoversWithLock(&duplicate)
		recoveryAtEnd := fixture.update.sequenceRecovery.seen &&
			fixture.update.sequenceRecovery.client == fixture.selected &&
			fixture.update.sequenceRecovery.ack == uploadEnd &&
			!duplicate.spansTcpRecoveryAck(fixture.update.sequenceRecovery.ack, len(dataPayload))
		fixture.update.stateLock.Unlock()
		if !covered || !recoveryAtEnd || fixture.parent.canSendPacket(&duplicate, fixture.update, fixture.selected) {
			t.Fatalf("upload duplicate lacks exact durable coverage: covered=%t recovery_at_end=%t", covered, recoveryAtEnd)
		}
		uploadQueued, uploadQueuedBytes, _ := fixture.sender.ResendQueueSize(fixture.receiver.ClientId(), MultiHopId{}, false, false)
		if uploadQueued != 0 || uploadQueuedBytes != 0 {
			t.Fatalf("upload still has Transfer custody: count=%d bytes=%d", uploadQueued, uploadQueuedBytes)
		}

		body := "abcdefghijklmnop"
		beforeReturnTerminals := fixture.returnTerminals.Load()
		if count, err := fixture.origin.Write([]byte(body)); err != nil || count != len(body) {
			t.Fatalf("actual mixed-ACK return flight: count=%d err=%v", count, err)
		}
		synctest.Wait()
		sequence := fixture.providerSequence.Load()
		if sequence == nil {
			t.Fatal("mixed-ACK fixture has no actual provider TCP owner")
		}
		// returnByteCount is charged allocation size; active payload lengths
		// independently prove the exact retained application bytes and range.
		returnSnapshot := func() (ByteCount, int, uint32, bool) {
			sequence.mutex.Lock()
			defer sequence.mutex.Unlock()
			payloadByteCount, frontier := 0, base
			contiguous := 0 <= sequence.returnHead && sequence.returnHead <= len(sequence.returnChunks)
			if contiguous {
				for _, chunk := range sequence.returnChunks[sequence.returnHead:] {
					if chunk.fin || chunk.start != frontier || chunk.end != chunk.start+uint32(len(chunk.payload)) {
						contiguous = false
					}
					payloadByteCount += len(chunk.payload)
					frontier = chunk.end
				}
			}
			return sequence.returnByteCount, payloadByteCount, frontier, contiguous
		}
		retainedCharge, retainedPayload, retainedFrontier, contiguous := returnSnapshot()
		sequence.mutex.Lock()
		duplicates, attempts := sequence.returnDuplicateAcks, sequence.returnAttempts
		sequence.mutex.Unlock()
		fixture.consumer.stateLock.Lock()
		frontier, packets, pending, received := fixture.consumer.next, fixture.consumer.packets, len(fixture.consumer.pending), fixture.consumer.received
		dropped, replays := fixture.consumer.dropped, fixture.consumer.replays
		pendingExact := true
		for index := 1; index < 4; index++ {
			offset := index * 4
			if fixture.consumer.pending[base+uint32(offset)] != body[offset:offset+4] {
				pendingExact = false
			}
		}
		fixture.consumer.stateLock.Unlock()
		returnQueued, returnQueuedBytes, _ := fixture.receiver.ResendQueueSize(fixture.sender.ClientId(), MultiHopId{}, false, false)
		if !dropped || replays != 0 || packets != 4 || frontier != base || pending != 3 || !pendingExact || received != "" ||
			retainedCharge <= 0 || retainedPayload != len(body) || !contiguous || retainedFrontier != base+uint32(len(body)) ||
			duplicates != 0 || attempts != 0 || returnQueued != 0 || returnQueuedBytes != 0 || fixture.returnTerminals.Load() <= beforeReturnTerminals {
			t.Fatalf("actual post-Transfer mixed-ACK premise: loss=%t replays=%d packets=%d frontier=%d base=%d pending=%d exact_pending=%t received=%q charge=%d payload=%d retained_frontier=%d contiguous=%t duplicates=%d attempts=%d transfer=%d/%d terminals=%d",
				dropped, replays, packets, frontier, base, pending, pendingExact, received, retainedCharge, retainedPayload,
				retainedFrontier, contiguous, duplicates, attempts, returnQueued, returnQueuedBytes, fixture.returnTerminals.Load()-beforeReturnTerminals)
		}

		ack := groupRecoveryPacket(uploadEnd, base, fixture.window, tcpFlagAck, nil)
		settings := fixture.parent.settings
		burstByteCount := ByteCount(len(data) + 3*len(ack))
		if settings.PacketGroupMaxPacketCount <= 0 || settings.PacketGroupMaxPacketCount < 4 ||
			settings.PacketGroupMaxByteCount <= 0 || settings.PacketGroupMaxByteCount < burstByteCount {
			t.Fatalf("mixed four-packet burst exceeds native bounds: count=%d bytes=%d burst=%d",
				settings.PacketGroupMaxPacketCount, settings.PacketGroupMaxByteCount, burstByteCount)
		}

		// Quiescence precedes both observers. The enqueue receipt reads only
		// stable parsed metadata, because original packet bytes may be returned
		// before it runs. The provider observer counts every actual IP dispatch.
		var admittedData, admittedAcks, unexpectedAdmissions, providerPackets atomic.Int64
		fixture.sender.settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
			group, ok := target.(*parsedPacketGroup)
			if !ok {
				unexpectedAdmissions.Add(1)
				return
			}
			for index := range group.packets {
				path := group.packets[index].ipPath
				if path == nil || path.Protocol != IpProtocolTcp || !path.Ack || path.Syn || path.Fin || path.Rst ||
					path.AckSequenceNumber != base || path.TcpWindowSize != fixture.window {
					unexpectedAdmissions.Add(1)
				} else if path.SequenceNumber == fixture.initial+1 && path.TcpPayloadByteCount == len(upload) {
					admittedData.Add(1)
				} else if path.SequenceNumber == uploadEnd && path.TcpPayloadByteCount == 0 {
					admittedAcks.Add(1)
				} else {
					unexpectedAdmissions.Add(1)
				}
			}
		}
		sequence.tcpBufferSettings.beforeSequenceSendForTest = func(actual *TcpSequence) {
			if actual != sequence {
				t.Error("mixed-ACK admission changed the actual provider TCP owner")
			}
			providerPackets.Add(1)
		}
		before, drops := time.Now(), fixture.parent.TcpCollapseDropCount()
		beforeSenderTerminals := fixture.senderTerminals.Load()
		beforeReplayTerminals := fixture.returnTerminals.Load()
		membership := []bool{true, true, true, true}
		accepted := offer(copyPackets(data, ack, ack, ack), membership)
		synctest.Wait()
		sequence.mutex.Lock()
		duplicates, attempts = sequence.returnDuplicateAcks, sequence.returnAttempts
		sequence.mutex.Unlock()
		fixture.consumer.stateLock.Lock()
		frontier, packets, pending, received = fixture.consumer.next, fixture.consumer.packets, len(fixture.consumer.pending), fixture.consumer.received
		replays = fixture.consumer.replays
		fixture.consumer.stateLock.Unlock()
		retainedCharge, retainedPayload, retainedFrontier, contiguous = returnSnapshot()
		ackQueued, ackQueuedBytes, _ := fixture.sender.ResendQueueSize(fixture.receiver.ClientId(), MultiHopId{}, false, false)
		replayQueued, replayQueuedBytes, _ := fixture.receiver.ResendQueueSize(fixture.sender.ClientId(), MultiHopId{}, false, false)
		membershipExact := throughUpgradeMux || slices.Equal(membership, []bool{false, true, true, true})
		if accepted != 3 || !membershipExact || admittedData.Load() != 0 || admittedAcks.Load() != 3 || unexpectedAdmissions.Load() != 0 ||
			providerPackets.Load() != 3 || duplicates < 3 || attempts != 1 || replays != 1 || packets != 5 ||
			frontier != base+uint32(len(body)) || pending != 0 || received != body ||
			retainedCharge <= 0 || retainedPayload != len(body) || !contiguous || retainedFrontier != base+uint32(len(body)) ||
			fixture.parent.TcpCollapseDropCount() != drops+1 || ackQueued != 0 || ackQueuedBytes != 0 || replayQueued != 0 || replayQueuedBytes != 0 ||
			fixture.senderTerminals.Load() <= beforeSenderTerminals || fixture.returnTerminals.Load() <= beforeReplayTerminals ||
			fixture.update.client.Load() != fixture.selected || fixture.parent.ctx.Err() != nil || time.Since(before) != 0 {
			t.Fatalf("NATIVE_MIXED_COVERED_DATA_ACK_STARVED_FAST_RECOVERY: entry=%s accepted=%d membership=%v membership_exact=%t admitted_data=%d admitted_acks=%d unexpected=%d provider_dispatches=%d duplicates=%d attempts=%d replays=%d packets=%d frontier=%d want_frontier=%d pending=%d received=%q charge=%d payload=%d retained_frontier=%d contiguous=%t collapse_drops=%d ack_transfer=%d/%d replay_transfer=%d/%d sender_terminals=%d return_terminals=%d elapsed=%s",
				entry, accepted, membership, membershipExact, admittedData.Load(), admittedAcks.Load(), unexpectedAdmissions.Load(),
				providerPackets.Load(), duplicates, attempts, replays, packets, frontier, base+uint32(len(body)), pending, received,
				retainedCharge, retainedPayload, retainedFrontier, contiguous, fixture.parent.TcpCollapseDropCount()-drops,
				ackQueued, ackQueuedBytes, replayQueued, replayQueuedBytes, fixture.senderTerminals.Load()-beforeSenderTerminals,
				fixture.returnTerminals.Load()-beforeReplayTerminals, time.Since(before))
		}

		// After the repair proof, release the exact retained origin range through
		// the same public native entry. Recovery has no extra upload authority.
		fixture.sender.settings.SendBufferSettings.afterGroupAdmissionForTest = nil
		beforeFinalTerminals := fixture.senderTerminals.Load()
		finalMembership := []bool{false}
		finalAck := groupRecoveryPacket(uploadEnd, base+uint32(len(body)), fixture.window, tcpFlagAck, nil)
		if count := offer(copyPackets(finalAck), finalMembership); count != 1 || !throughUpgradeMux && !finalMembership[0] {
			t.Fatalf("mixed-ACK repair's final cumulative ACK refused: entry=%s count=%d membership=%v", entry, count, finalMembership)
		}
		synctest.Wait()
		retainedCharge, retainedPayload, _, _ = returnSnapshot()
		finalQueued, finalQueuedBytes, _ := fixture.sender.ResendQueueSize(fixture.receiver.ClientId(), MultiHopId{}, false, false)
		if retainedCharge != 0 || retainedPayload != 0 || providerPackets.Load() != 4 || finalQueued != 0 || finalQueuedBytes != 0 ||
			fixture.senderTerminals.Load() <= beforeFinalTerminals || time.Since(before) != 0 {
			t.Fatalf("mixed-ACK repair's final ACK lost exact release: entry=%s charge=%d payload=%d provider_dispatches=%d transfer=%d/%d terminals=%d elapsed=%s",
				entry, retainedCharge, retainedPayload, providerPackets.Load(), finalQueued, finalQueuedBytes,
				fixture.senderTerminals.Load()-beforeFinalTerminals, time.Since(before))
		}
	})
}
