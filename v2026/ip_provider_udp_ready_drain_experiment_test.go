package connect

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// This test-only adapter asks the existing H1 ready-drain coalescer for its
// stream-sized envelope while the real legacy P2P writer and four-slot route
// remain in place. It is an upper-bound experiment, NOT an approved P2P policy:
// production P2P still needs its conservative mixed/fast-carrier limit, and
// larger messages can retain more bytes in the same number of route slots.
type providerUdpStreamDrainExperimentTransport struct{ Transport }

func (*providerUdpStreamDrainExperimentTransport) TransportType() TransportType {
	return TransportTypeH1
}

type providerUdpDrainExperimentResult struct {
	admitted, refused, wireWrites, wireBytes int
	stallPeakRootBytes, peakRootBytes        ByteCount
	drainTime                                time.Duration
}

// The service model charges actual encoded bytes at 5 Mbit/s plus one explicit
// 250-us handoff per physical write. Its virtual drain time is NOT a measured
// SCTP throughput estimate. Admission before the 121-ms barrier is the actual
// provider/Transfer/P2P result, independent of this post-barrier service model.
func runProviderUdpDrainExperiment(t *testing.T, streamEnvelope bool) (result providerUdpDrainExperimentResult) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		clientSettings := DefaultClientSettings()
		clientSettings.EncryptionSettings.Mode = EncryptionModeOff
		provider, client, _ := newProviderTransferKeyTestFixtureWithClientSettings(
			t, DefaultRemoteUserNatProviderSettings(), clientSettings,
		)
		defer closeTransferGroupTestClient(t, client)
		defer provider.Close()
		peerId := NewId()
		client.ContractManager().AddNoContractPeer(peerId)
		ctx, cancel := context.WithCancel(provider.ctx)
		defer cancel()
		settings := DefaultP2pTransportSettings()
		settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
		settings.LegacySendQueueByteCount = 0 // isolate historical ready-drain envelope
		conn := &p2pProbePressureConn{ctx: ctx}
		const offeredBitRate = 3_750_000
		const interval = time.Duration(8 * 1000 * int64(time.Second) / offeredBitRate)
		const stall = 121 * time.Millisecond
		const offerCount = int((stall-1)/interval) + 1
		var wirePackets [offerCount + 1]bool
		var sourcePackets [offerCount + 1]bool
		writtenPackets, wireWrites, wireBytes := 0, 0, 0
		measuring := false
		rootBaseline := ByteCount(0)
		var peakRoots atomic.Int64
		sampleRoots := func() {
			value := int64(MessagePoolOutstandingByteCount() - rootBaseline)
			for previous := peakRoots.Load(); previous < value; previous = peakRoots.Load() {
				if peakRoots.CompareAndSwap(previous, value) {
					break
				}
			}
		}
		drained := make(chan struct{})
		conn.onWire = func(wire []byte) {
			if measuring {
				sampleRoots()
				time.Sleep(250*time.Microsecond + time.Duration(len(wire)*8*int(time.Second)/5_000_000))
			}
			pack := decodeCompactContractTestPack(t, wire)
			if !pack.GetNack() {
				t.Error("UDP unexpectedly entered reliable Transfer delivery")
			}
			for _, frame := range pack.Frames {
				packet := frame.MessageBytes
				index := int(binary.BigEndian.Uint32(packet[len(packet)-4:]))
				if index < 0 || len(wirePackets) <= index || wirePackets[index] {
					t.Errorf("duplicated or invalid UDP identity %d", index)
					continue
				}
				wirePackets[index] = true
				writtenPackets++
			}
			if measuring {
				wireWrites++
				wireBytes += len(wire)
				if writtenPackets == 1+result.admitted {
					close(drained)
				}
			}
		}
		physical, route := newP2pSendTransportForPeer(ctx, cancel, conn, peerId, NewId(), settings, false, nil)
		advertised := physical
		if streamEnvelope {
			advertised = &providerUdpStreamDrainExperimentTransport{physical}
		}
		client.RouteManager().UpdateTransport(advertised, []Route{route})
		defer func() {
			client.RouteManager().RemoveTransport(advertised)
			if err := physical.(P2pRouteLifecycle).CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		completed := make(chan providerReturnSendResult, 1)
		provider.afterReturnSendForTest = func(result providerReturnSendResult) { completed <- result }
		template := craftSecurityPacket(IpProtocolUdp, net.ParseIP("203.0.113.7"), 8080,
			net.ParseIP("10.0.0.9"), 42001, false, make([]byte, 1000))
		ipPath, err := ParseIpPath(template)
		if err != nil {
			t.Fatal(err)
		}
		key := TransferKey{ForceStream: true, EncryptionRole: protocol.SequenceRole_SequenceRoleServer}
		offer := func(index int) bool {
			// Each offered datagram has a distinct root. Reusing one borrowed
			// packet for all offers would understate retained memory via shares.
			packet := MessagePoolCopy(template)
			binary.BigEndian.PutUint32(packet[len(packet)-4:], uint32(index))
			before := time.Now()
			provider.receiveTransfer(SourceId(peerId), key, protocol.ProvideMode_Public, ipPath, packet)
			MessagePoolReturn(packet)
			synctest.Wait()
			select {
			case completion := <-completed:
				if time.Now() != before || completion.packetCount != 1 || completion.packetByteCount != ByteCount(len(template)) {
					t.Fatalf("nonblocking source changed its contract: elapsed=%s result=%+v", time.Since(before), completion)
				}
				if measuring {
					sampleRoots()
				}
				sourcePackets[index] = completion.sent
				return completion.sent
			default:
				t.Fatal("source did not publish its immediate terminal disposition")
				return false
			}
		}
		if !offer(0) || writtenPackets != 1 {
			t.Fatal("warm packet did not establish the ready lane")
		}
		rootBaseline = MessagePoolOutstandingByteCount()
		measuring = true
		gate := make(chan struct{})
		conn.mutex.Lock()
		conn.gate = gate
		conn.mutex.Unlock()
		for index := range offerCount {
			if offer(index + 1) {
				result.admitted++
			} else {
				result.refused++
			}
			if index+1 < offerCount {
				time.Sleep(interval)
			}
		}
		time.Sleep(stall - time.Duration(offerCount-1)*interval)
		result.stallPeakRootBytes = ByteCount(peakRoots.Load())
		if result.refused == 0 || writtenPackets != 1 || len(route) != cap(route) || cap(route) != 4 || clientSettings.SendBufferSettings.SequenceBufferSize != 32 {
			t.Fatalf("experiment did not preserve the stalled bounded pipeline: result=%+v route=%d/%d written=%d", result, len(route), cap(route), writtenPackets)
		}
		releaseTime := time.Now()
		close(gate)
		select {
		case <-drained:
		case <-time.After(time.Second):
			t.Fatal("admitted packets did not drain after carrier release")
		}
		synctest.Wait()
		result.drainTime = time.Since(releaseTime)
		result.wireWrites, result.wireBytes = wireWrites, wireBytes
		result.peakRootBytes = ByteCount(peakRoots.Load())
		if wirePackets != sourcePackets {
			t.Fatal("wire identities do not exactly match admitted source identities")
		}
		drops := provider.CongestionDropStats()
		if drops.ReturnQueuePacketCount != 0 || drops.ReturnSendPacketCount != int64(result.refused) || drops.ReturnSendByteCount != ByteCount(result.refused*len(template)) {
			t.Fatalf("source refusal accounting changed: %+v result=%+v", drops, result)
		}
	})
	return
}

// Even an existing wider stream coalescer cannot save offers refused while
// its sequence is parked at a full carrier. The model deliberately checks
// admission separately from the faster ready-only drain after release.
func TestProviderUdpReadyDrainCannotRescueStalledSource(t *testing.T) {
	assertMessagePoolOwnership(t)
	var results [2]providerUdpDrainExperimentResult
	for index, name := range []string{"production-p2p-envelope", "stream-envelope-upper-bound"} {
		t.Run(name, func(t *testing.T) {
			results[index] = runProviderUdpDrainExperiment(t, index != 0)
			t.Logf("%+v", results[index])
		})
	}
	baseline, candidate := results[0], results[1]
	if candidate.admitted != baseline.admitted || candidate.refused != baseline.refused {
		t.Fatalf("post-stall batching changed pre-release admission: baseline=%+v candidate=%+v", baseline, candidate)
	}
	if baseline.wireWrites <= candidate.wireWrites || baseline.drainTime <= candidate.drainTime {
		t.Fatalf("experiment did not exercise ready-only coalescing: baseline=%+v candidate=%+v", baseline, candidate)
	}
	// Before release the admitted working set is identical. After release,
	// wider wire roots can overlap packet roots differently as goroutines run;
	// those sampled peaks are measurements, not a memory-neutrality guarantee.
	// In particular, race instrumentation can expose overlap missed by the
	// earlier normal cohort. Never turn this negative experiment into a claim
	// that unchanged queue slots prove an unchanged retained-byte ceiling.
	if baseline.stallPeakRootBytes != candidate.stallPeakRootBytes {
		t.Fatalf("candidate changed pre-release pooled-root retention: baseline=%+v candidate=%+v", baseline, candidate)
	}
}

// CPU-only encoder experiment: the same 30 already-owned UDP packets are
// serialized individually or three per established stream envelope. This
// excludes routing, SCTP, loss, encryption, callbacks, and device memory, and
// cannot qualify a production change by itself. Peak wire root size is
// reported explicitly: fewer calls do not make larger route slots free.
func BenchmarkProviderUdpReadyDrainWireEncoding(b *testing.B) {
	for _, groupSize := range []int{1, 3} {
		b.Run(fmt.Sprintf("packets-per-wire=%d", groupSize), func(b *testing.B) {
			const packetCount = 30
			packet := craftSecurityPacket(IpProtocolUdp, net.ParseIP("203.0.113.7"), 8080,
				net.ParseIP("10.0.0.9"), 42001, false, make([]byte, 1000))
			var frames [packetCount]*protocol.Frame
			for index := range frames {
				frames[index] = &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: packet, Raw: true}
			}
			pack := sendPackFrame{path: DestinationId(NewId()), messageId: NewId(), sequenceId: NewId(), nack: true}
			pack.frames = frames[:groupSize]
			warm := marshalSendPackTransferFrame(&pack)
			rootBytes, wireBytes := MessagePoolRootByteCount(warm), len(warm)
			MessagePoolReturn(warm)
			b.ReportAllocs()
			b.SetBytes(packetCount * 1000)
			b.ResetTimer()
			for range b.N {
				for start := 0; start < packetCount; start += groupSize {
					pack.frames = frames[start : start+groupSize]
					wire := marshalSendPackTransferFrame(&pack)
					MessagePoolReturn(wire)
				}
			}
			b.ReportMetric(float64(packetCount/groupSize), "wire-writes/op")
			b.ReportMetric(float64(packetCount/groupSize*wireBytes), "wire-B/op")
			b.ReportMetric(float64(rootBytes), "peak-wire-root-B")
		})
	}
}
