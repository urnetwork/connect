// These tests force partial acknowledgements after a lost return burst and
// distinguish its recovery frontier from newly issued data and silent peers.
package connect

import (
	"bytes"
	"context"
	"math"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Drives only the dedicated replay worker: all initial bytes were already
// delivered by Transfer and then lost at the inner TCP receiver's boundary.
func testTcpReturnBurstRecovery(t *testing.T, initial uint32, ipVersion int, timerRecovery bool, appendNewData bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultTcpBufferSettingsWithBufferSize(16)
	settings.Mtu = 44
	settings.Log = NewNoopLogger()
	sourceIp := net.IPv4(192, 0, 2, 2).To4()
	destinationIp := net.IPv4(198, 51, 100, 2).To4()
	if ipVersion == 6 {
		settings.Mtu += Ipv6HeaderSize - Ipv4HeaderSizeWithoutExtensions
		sourceIp = net.ParseIP("2001:db8::2")
		destinationIp = net.ParseIP("2001:db8:1::2")
	}
	want := []byte("lost-burst-tail!")
	base := initial + 1
	var received []byte
	var sequence *TcpSequence
	packetCount := 0
	sequence = NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		_, source, destination, transport, ok := parseIpv4(packet)
		if ipVersion == 6 {
			_, source, destination, transport, ok = parseIpv6(packet)
		}
		var tcp parsedTcp
		if !ok || !parseTcpPacket(source, destination, transport, &tcp) {
			t.Error("invalid replay packet")
			return
		}
		packetCount += 1
		if tcp.seq != base+uint32(len(received)) || len(tcp.payload) != 4 {
			t.Errorf("replay seq=%d bytes=%d, want seq=%d bytes=4", tcp.seq, len(tcp.payload), base+uint32(len(received)))
			return
		}
		received = append(received, tcp.payload...)
		if appendNewData && packetCount == 1 {
			sequence.mutex.Lock()
			sequence.receiveSeq += 4
			sequence.mutex.Unlock()
			if !sequence.retainReturnChunk([]byte("next"), base+uint32(len(want)), false) {
				t.Error("new return data was not retained")
			}
		}
		sequence.mutex.Lock()
		sequence.applySendAckWithLock(&parsedTcp{
			ack: true, ackNumber: base + uint32(len(received)), windowSize: math.MaxUint16,
		})
		sequence.mutex.Unlock()
	}, SourceId(NewId()), protocol.ProvideMode_Network, ipVersion,
		sourceIp, 41000, destinationIp, 443, initial, settings)
	sequence.receiveSeqAck = base
	sequence.receiveSeq = base + uint32(len(want))
	if !sequence.retainReturnChunk(want, base, false) {
		t.Fatal("lost burst was not retained")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sequence.runReturnRecovery()
	}()
	defer func() {
		cancel()
		<-done
		sequence.releaseReturnChunks()
		sequence.memory.release()
	}()
	if timerRecovery {
		// Virtual time triggers exactly the existing initial resend timer.
		synctest.Wait()
		time.Sleep(settings.ReturnResendTimeout)
	} else {
		sequence.mutex.Lock()
		for range 3 {
			sequence.applySendAckWithLock(&parsedTcp{ack: true, ackNumber: base, windowSize: math.MaxUint16})
		}
		sequence.mutex.Unlock()
	}
	synctest.Wait()
	if !bytes.Equal(received, want) {
		t.Fatalf("partial ACK stopped burst recovery at %d/%d bytes; no second timer should be needed", len(received), len(want))
	}
	if packetCount != len(want)/4 {
		t.Fatalf("recovery emitted %d packets, want %d", packetCount, len(want)/4)
	}
	if appendNewData && sequence.returnHead == len(sequence.returnChunks) {
		t.Fatal("recovery of the original flight consumed newly issued data")
	}
}

// Every advancing partial ACK repairs the next hole without another timeout,
// including wraparound sequence arithmetic and both IP packet encodings.
func TestTcpReturnBurstRecoveryFollowsPartialAcks(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, testCase := range []struct {
		initial   uint32
		ipVersion int
	}{
		{initial: 100, ipVersion: 4},
		{initial: math.MaxUint32 - 5, ipVersion: 4},
		{initial: 100, ipVersion: 6},
	} {
		synctest.Test(t, func(t *testing.T) {
			testTcpReturnBurstRecovery(t, testCase.initial, testCase.ipVersion, false, false)
		})
	}
}

// Tail loss has no duplicate ACK train, but one initial timer must suffice to
// start acknowledgement-paced repair of the entire retained burst.
func TestTcpReturnTailBurstRecoveryNeedsOnlyInitialTimer(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		testTcpReturnBurstRecovery(t, 100, 4, true, false)
	})
}

// New origin bytes cannot extend an existing recovery episode indefinitely or
// be replayed before their ordinary acknowledgement deadline.
func TestTcpReturnBurstRecoveryStopsAtOriginalFlight(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		testTcpReturnBurstRecovery(t, 100, 4, false, true)
	})
}

// A silent peer supplies no progress credit and retains the existing resend
// backoff instead of turning the partial-ACK repair loop into a busy sender.
func TestTcpReturnBurstRecoveryPreservesSilentPeerBackoff(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		settings.Mtu = 44
		settings.Log = NewNoopLogger()
		var packetCount atomic.Int32
		sequence := NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, _ []byte) {
			packetCount.Add(1)
		}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
			net.IPv4(192, 0, 2, 2).To4(), 41000, net.IPv4(198, 51, 100, 2).To4(), 443, 100, settings)
		sequence.receiveSeqAck = 101
		sequence.receiveSeq = 117
		if !sequence.retainReturnChunk([]byte("lost-burst-tail!"), 101, false) {
			t.Fatal("lost burst was not retained")
		}
		done := make(chan struct{})
		go func() { defer close(done); sequence.runReturnRecovery() }()
		defer func() {
			cancel()
			<-done
			sequence.releaseReturnChunks()
			sequence.memory.release()
		}()
		time.Sleep(settings.ReturnResendTimeout)
		synctest.Wait()
		if count := packetCount.Load(); count != 1 {
			t.Fatalf("first timeout emitted %d packets, want 1", count)
		}
		time.Sleep(settings.ReturnResendTimeout)
		synctest.Wait()
		if count := packetCount.Load(); count != 1 {
			t.Fatalf("silent peer ignored doubled timeout: packets=%d", count)
		}
		time.Sleep(settings.ReturnResendTimeout)
		synctest.Wait()
		if count := packetCount.Load(); count != 2 {
			t.Fatalf("second timeout emitted %d packets, want 2 total", count)
		}
	})
}
