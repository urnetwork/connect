// A bounded return-data reorder barrier exercises fast repair through a fitting
// native acknowledgement burst without depending on an admission implementation.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Six real, disjoint return segments yield six unchanged acknowledgements in the
// existing synthetic inner consumer. Every acknowledgement fits the ordinary
// native group bounds and must reach the provider before a recovery timer.
func TestNativeDuplicateAckReorderedBurstPreservesFastRecovery(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeDuplicateAckFixture(t, false)
		sequence := fixture.providerSequence.Load()
		if sequence == nil {
			t.Fatal("reordered fixture has no provider TCP owner")
		}
		// The constructor's final quiescence precedes this per-packet dispatch
		// observer. Transfer frame counts can differ from IP member counts.
		var providerPackets atomic.Int64
		sequence.tcpBufferSettings.beforeSequenceSendForTest = func(actual *TcpSequence) {
			if actual != sequence {
				t.Error("reordered acknowledgement changed provider TCP owner")
			}
			providerPackets.Add(1)
		}
		const segmentByteCount = 4
		const flightPacketCount = 12
		body := "abcdefghijklmnopqrstuvwxABCDEFGHIJKLMNOPQRSTUVWX"
		if len(body) != segmentByteCount*flightPacketCount || int(fixture.nat.settings.TcpBufferSettings.Mtu) != int(Ipv4HeaderSizeWithoutExtensions+TcpHeaderSizeWithoutExtensions)+segmentByteCount {
			t.Fatal("fixture must retain its actual twelve four-byte return segments")
		}
		fixture.consumer.stateLock.Lock()
		base := fixture.consumer.base
		fixture.consumer.stateLock.Unlock()

		// Each callback packet is borrowed. The barrier owns exactly one shared
		// copy for each held original, released after forwarding or joined close.
		type retainedReturnPacket struct {
			client        *multiClientChannel
			source        TransferPath
			provideMode   protocol.ProvideMode
			transportType TransportType
			packet        []byte
		}
		var stateLock sync.Mutex
		var retainedReturnPackets []retainedReturnPacket
		var seenOriginal [flightPacketCount]bool
		originalCount, forwardedOriginalCount, headReplays := 0, 0, 0
		originalReceive := fixture.selected.clientReceivePacketCallback
		fixture.selected.clientReceivePacketCallback = func(client *multiClientChannel, source TransferPath, provideMode protocol.ProvideMode, transportType TransportType, path *IpPath, packet []byte) {
			if path == nil || path.Protocol != IpProtocolTcp || path.TcpPayloadByteCount == 0 {
				originalReceive(client, source, provideMode, transportType, path, packet)
				return
			}
			offset := int32(path.SequenceNumber - base)
			if path.TcpPayloadByteCount != segmentByteCount || offset < 0 || offset%segmentByteCount != 0 || offset >= segmentByteCount*flightPacketCount {
				t.Error("actual provider return escaped the bounded reorder flight")
				originalReceive(client, source, provideMode, transportType, path, packet)
				return
			}
			index := int(offset / segmentByteCount)
			hold := false
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				if !seenOriginal[index] {
					seenOriginal[index] = true
					originalCount++
					if index%2 == 0 {
						retainedReturnPackets = append(retainedReturnPackets, retainedReturnPacket{
							client:        client,
							source:        source,
							provideMode:   provideMode,
							transportType: transportType,
							packet:        MessagePoolShareReadOnly(packet),
						})
						hold = true
					} else {
						forwardedOriginalCount++
					}
				} else if index == 0 {
					headReplays++
				}
			}()
			if !hold {
				originalReceive(client, source, provideMode, transportType, path, packet)
			}
		}
		defer func() {
			fixture.close()
			stateLock.Lock()
			remaining := retainedReturnPackets
			retainedReturnPackets = nil
			stateLock.Unlock()
			for _, retained := range remaining {
				MessagePoolReturn(retained.packet)
			}
		}()

		beforeReturnTerminals := fixture.returnTerminals.Load()
		if count, err := fixture.origin.Write([]byte(body)); err != nil || count != len(body) {
			t.Fatalf("actual reordered origin flight: count=%d err=%v", count, err)
		}
		synctest.Wait()
		stateLock.Lock()
		originals, forwarded, held, replays := originalCount, forwardedOriginalCount, len(retainedReturnPackets), headReplays
		stateLock.Unlock()
		fixture.consumer.stateLock.Lock()
		frontier, packets, buffered, received := fixture.consumer.next, fixture.consumer.packets, len(fixture.consumer.pending), fixture.consumer.received
		disjointPayloads := true
		for index := 0; index < flightPacketCount/2; index++ {
			offset := (2*index + 1) * segmentByteCount
			if fixture.consumer.pending[base+uint32(offset)] != body[offset:offset+segmentByteCount] {
				disjointPayloads = false
			}
		}
		fixture.consumer.stateLock.Unlock()
		sequence.mutex.Lock()
		retainedCharge, attempts := sequence.returnByteCount, sequence.returnAttempts
		retainedPayloadByteCount := 0
		retainedFrontier := base
		retainedContiguous := 0 <= sequence.returnHead && sequence.returnHead <= len(sequence.returnChunks)
		if retainedContiguous {
			for _, chunk := range sequence.returnChunks[sequence.returnHead:] {
				if chunk.fin || chunk.start != retainedFrontier || chunk.end != chunk.start+uint32(len(chunk.payload)) {
					retainedContiguous = false
				}
				retainedPayloadByteCount += len(chunk.payload)
				retainedFrontier = chunk.end
			}
		}
		sequence.mutex.Unlock()
		queued, queuedBytes, _ := fixture.receiver.ResendQueueSize(fixture.sender.ClientId(), MultiHopId{}, false, false)
		if originals != 12 || forwarded != 6 || held != 6 || replays != 0 || frontier != base || packets != 6 || buffered != 6 || !disjointPayloads || received != "" ||
			retainedCharge <= 0 || retainedPayloadByteCount != len(body) || !retainedContiguous || retainedFrontier != base+uint32(len(body)) ||
			attempts != 0 || queued != 0 || queuedBytes != 0 || fixture.returnTerminals.Load() <= beforeReturnTerminals {
			t.Fatalf("real reordered post-Transfer premise: originals=%d forwarded=%d held=%d replays=%d frontier=%d base=%d packets=%d buffered=%d disjoint_payloads=%t received=%q charge=%d retained_payload=%d retained_frontier=%d contiguous=%t attempts=%d transfer=%d/%d terminals=%d",
				originals, forwarded, held, replays, frontier, base, packets, buffered, disjointPayloads, received, retainedCharge, retainedPayloadByteCount, retainedFrontier, retainedContiguous, attempts, queued, queuedBytes, fixture.returnTerminals.Load()-beforeReturnTerminals)
		}
		// These acknowledgements are generated for the six actual arrivals above
		// by the same labelled synthetic inner consumer as the existing fixture.
		ack := groupRecoveryPacket(fixture.initial+1, base, fixture.window, tcpFlagAck, nil)
		settings := fixture.parent.settings
		if settings.PacketGroupMaxPacketCount <= 0 || settings.PacketGroupMaxPacketCount < 6 ||
			settings.PacketGroupMaxByteCount <= 0 || settings.PacketGroupMaxByteCount < ByteCount(6*len(ack)) {
			t.Fatalf("six-packet acknowledgement burst exceeds native group bounds: count=%d bytes=%d burst_bytes=%d", settings.PacketGroupMaxPacketCount, settings.PacketGroupMaxByteCount, 6*len(ack))
		}
		before := time.Now()
		drops := fixture.parent.TcpCollapseDropCount()
		beforeProviderPackets := providerPackets.Load()
		beforeSenderTerminals := fixture.senderTerminals.Load()
		owned := make([][]byte, 6)
		membership := make([]bool, len(owned))
		for index := range owned {
			owned[index] = MessagePoolCopy(ack)
		}
		accepted := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()),
			protocol.ProvideMode_Network, owned, 5*time.Second, membership)
		actual := 0
		for _, admitted := range membership {
			if admitted {
				actual++
			}
		}
		if actual != accepted {
			t.Fatalf("native reordered ACK burst lost result membership: count=%d accepted=%v", accepted, membership)
		}
		synctest.Wait()
		sequence.mutex.Lock()
		duplicates, attempts := sequence.returnDuplicateAcks, sequence.returnAttempts
		sequence.mutex.Unlock()
		stateLock.Lock()
		replays = headReplays
		stateLock.Unlock()
		fixture.consumer.stateLock.Lock()
		frontier = fixture.consumer.next
		fixture.consumer.stateLock.Unlock()
		elapsed := time.Since(before)
		providerDispatches := providerPackets.Load() - beforeProviderPackets
		ackQueued, ackQueuedBytes, _ := fixture.sender.ResendQueueSize(fixture.receiver.ClientId(), MultiHopId{}, false, false)
		if accepted != 6 || providerDispatches != 6 || duplicates < 3 || attempts != 1 || replays != 1 || frontier != base+2*segmentByteCount || elapsed != 0 ||
			ackQueued != 0 || ackQueuedBytes != 0 || fixture.senderTerminals.Load() <= beforeSenderTerminals {
			t.Fatalf("NATIVE_DUPLICATE_ACK_REORDERED_BURST_STARVED_FAST_RECOVERY: accepted=%d membership=%v provider_dispatches=%d provider_duplicates=%d attempts=%d replays=%d frontier=%d want=%d elapsed=%s transfer=%d/%d terminals=%d collapse_drops=%d",
				accepted, membership, providerDispatches, duplicates, attempts, replays, frontier, base+2*segmentByteCount, elapsed, ackQueued, ackQueuedBytes, fixture.senderTerminals.Load()-beforeSenderTerminals, fixture.parent.TcpCollapseDropCount()-drops)
		}

		// Release the six delayed originals only after proving fast repair. The
		// replayed head is now a harmless duplicate; all original bytes still
		// pass through the actual Multi callback before final cumulative release.
		for range flightPacketCount / 2 {
			stateLock.Lock()
			retained := retainedReturnPackets[0]
			retainedReturnPackets[0] = retainedReturnPacket{}
			retainedReturnPackets = retainedReturnPackets[1:]
			stateLock.Unlock()
			func() {
				defer MessagePoolReturn(retained.packet)
				path, err := ParseIpPath(retained.packet)
				if err != nil {
					t.Fatalf("retained actual return packet: %v", err)
				}
				originalReceive(retained.client, retained.source, retained.provideMode, retained.transportType, path, retained.packet)
			}()
		}
		synctest.Wait()
		fixture.consumer.stateLock.Lock()
		received = fixture.consumer.received
		fixture.consumer.stateLock.Unlock()
		if received != body {
			t.Fatalf("reordered native repair lost origin bytes: got=%q want=%q", received, body)
		}
		beforeFinalTerminals := fixture.senderTerminals.Load()
		if !fixture.offer(groupRecoveryPacket(fixture.initial+1, base+uint32(len(body)), fixture.window, tcpFlagAck, nil)) {
			t.Fatal("reordered flight's final cumulative ACK was refused")
		}
		synctest.Wait()
		sequence.mutex.Lock()
		remaining := sequence.returnByteCount
		sequence.mutex.Unlock()
		finalQueued, finalQueuedBytes, _ := fixture.sender.ResendQueueSize(fixture.receiver.ClientId(), MultiHopId{}, false, false)
		finalProviderDispatches := providerPackets.Load() - beforeProviderPackets
		if remaining != 0 || finalProviderDispatches != 7 || finalQueued != 0 || finalQueuedBytes != 0 ||
			fixture.senderTerminals.Load() <= beforeFinalTerminals || time.Since(before) != 0 {
			t.Fatalf("reordered flight's final ACK lost exact provider release: retained_charge=%d provider_dispatches=%d transfer=%d/%d terminals=%d elapsed=%s",
				remaining, finalProviderDispatches, finalQueued, finalQueuedBytes, fixture.senderTerminals.Load()-beforeFinalTerminals, time.Since(before))
		}
	})
}
