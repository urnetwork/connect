// Oversized socket datagrams must never become checksum-valid truncated replies.
// The portable controls use real UDP sockets and join the reader before inspection.
package connect

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Hides SyscallConn while preserving the real datagram socket's Read contract.
type mtuOpaqueUdpConn struct {
	net.Conn
}

// One reader owns this collector until Run joins. Completed payloads are copied
// only by the fixture; every production packet owner is returned through the gate.
type mtuUdpReadCollector struct {
	gate        ipFragmentGate
	source      TransferPath
	payloads    [][]byte
	prepared    []int
	aborted     int
	err         error
	healthy     chan struct{}
	healthyOnce sync.Once
}

// Takes each packet through the real reassembler and records a complete datagram.
func (self *mtuUdpReadCollector) take(packets [][]byte) {
	for _, packet := range packets {
		if DefaultMtu < len(packet) && self.err == nil {
			self.err = fmt.Errorf("packet length %d exceeds MTU %d", len(packet), DefaultMtu)
		}
		result := self.gate.processOwned(self.source, TransferKey{}, protocol.ProvideMode_Network, packet)
		if !result.accepted && self.err == nil {
			self.err = fmt.Errorf("return fragment was rejected")
		}
		if result.packet != nil {
			kind, sourceIp, destinationIp, transport, ok := parseIpv4(result.packet)
			var udp parsedUdp
			if !ok || kind != ipProtocolNumberUdp ||
				!parseUdpPacket(sourceIp, destinationIp, transport, &udp) ||
				transportChecksum(kind, sourceIp, destinationIp, transport) != 0 {
				if self.err == nil {
					self.err = fmt.Errorf("returned datagram is malformed")
				}
			} else {
				self.payloads = append(self.payloads, bytes.Clone(udp.payload))
				if bytes.Equal(udp.payload, []byte("healthy-after-oversize")) {
					self.healthyOnce.Do(func() { close(self.healthy) })
				}
			}
		}
		returnIpFragmentProcessResult(result)
	}
}

// Holds precisely the admitted read's callback and retires it on both outcomes.
type mtuUdpReadConsumer struct {
	collector *mtuUdpReadCollector
	released  func()
}

// Takes all packet owners, then releases the existing return-read lease.
func (self *mtuUdpReadConsumer) commit(packets [][]byte) {
	defer self.released()
	self.collector.take(packets)
}

// No packet was accepted, so only the prepared lease needs retirement.
func (self *mtuUdpReadConsumer) abort() {
	self.collector.aborted++
	self.released()
}

// Real sockets exercise both pre-read peeking and the non-SyscallConn fallback.
// A final valid datagram is the delivery barrier, including under the old bug.
func TestUdpPortableReaderRejectsOversizedDatagram(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, opaque := range []bool{false, true} {
		for _, admitted := range []bool{false, true} {
			func() {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				defer socket.Close()
				sender, err := net.DialUDP("udp4", nil, socket.LocalAddr().(*net.UDPAddr))
				if err != nil {
					t.Fatal(err)
				}
				defer sender.Close()
				settings := DefaultUdpBufferSettingsWithBufferSize(1)
				settings.MemoryBudget = NewTransferMemoryBudget(256 * 1024)
				settings.WriteBatchSize = 1
				settings.Log = NewNoopLogger()
				settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
					if opaque {
						return &mtuOpaqueUdpConn{Conn: socket}, nil
					}
					return socket, nil
				}}
				collector := &mtuUdpReadCollector{source: SourceId(NewId()), healthy: make(chan struct{})}
				defer collector.gate.close()
				sequence := NewUdpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
					collector.take([][]byte{MessagePoolShareReadOnly(packet)})
				}, collector.source, protocol.ProvideMode_Network, 4,
					net.IPv4(192, 0, 2, 1).To4(), 42000, net.IPv4(127, 0, 0, 1).To4(), 44000, settings)
				if sequence == nil {
					t.Fatal("portable flow was refused")
				}
				if admitted {
					sequence.prepareReturnReadCallback = func(_ *UdpSequence, size int, released func()) (udpReturnReadConsumer, bool) {
						collector.prepared = append(collector.prepared, size)
						return &mtuUdpReadConsumer{collector: collector, released: released}, true
					}
				}
				joined := make(chan struct{})
				go func() { defer close(joined); sequence.Run() }()
				join := func() {
					sequence.Close()
					select {
					case <-joined:
					case <-time.After(5 * time.Second):
						t.Fatal("portable UDP reader did not join")
					}
				}
				defer join()
				maximum := settings.ReadBufferByteCount
				wanted := [][]byte{fragmentTestPayload(maximum), []byte("healthy-after-oversize")}
				for _, payload := range [][]byte{fragmentTestPayload(maximum + 1), fragmentTestPayload(2 * maximum), wanted[0], wanted[1]} {
					if n, err := sender.Write(payload); err != nil || n != len(payload) {
						t.Fatalf("opaque=%t admitted=%t write=%d/%d err=%v", opaque, admitted, n, len(payload), err)
					}
				}
				select {
				case <-collector.healthy:
				case <-ctx.Done():
					t.Fatalf("opaque=%t admitted=%t healthy delivery: %v", opaque, admitted, ctx.Err())
				}
				if sequence.ctx.Err() != nil {
					t.Fatal("oversize terminated a healthy flow")
				}
				join()
				if collector.err != nil {
					t.Fatal(collector.err)
				}
				if len(collector.payloads) != len(wanted) {
					t.Fatalf("opaque=%t admitted=%t delivered %d datagrams, want %d; a truncated prefix escaped", opaque, admitted, len(collector.payloads), len(wanted))
				}
				for i, payload := range wanted {
					if !bytes.Equal(collector.payloads[i], payload) {
						t.Fatalf("opaque=%t admitted=%t payload %d changed", opaque, admitted, i)
					}
				}
				if admitted && !opaque && (len(collector.prepared) != 2 || collector.prepared[0] != maximum || collector.prepared[1] != len(wanted[1]) || collector.aborted != 0) {
					t.Fatalf("peek prepared oversized credit: sizes=%v aborts=%d", collector.prepared, collector.aborted)
				}
				if admitted && opaque && collector.aborted < 2 {
					t.Fatal("opaque oversized reads did not abort their admitted leases")
				}
				if sequence.returnReadPending.Load() || settings.MemoryBudget.UsedByteCount() != 0 {
					t.Fatal("joined portable reader retained a lease or flow budget")
				}
			}()
		}
	}
}

// The sentinel is one prepaid byte, with a protocol-size clamp even for an
// extreme caller setting. A failed constructor cannot retain part of a claim.
func TestUdpOversizeScratchAdmission(t *testing.T) {
	for _, row := range []struct {
		maximum int
		scratch int
	}{
		{maximum: -1, scratch: 1},
		{maximum: 0, scratch: 1},
		{maximum: 1, scratch: 2},
		{maximum: 2048, scratch: 2049},
		{maximum: 4096, scratch: 4097},
		{maximum: 65527, scratch: 65528},
		{maximum: int(^uint(0) >> 1), scratch: 65528},
	} {
		if got := udpReadScratchByteCount(row.maximum); got != row.scratch {
			t.Fatalf("maximum=%d scratch=%d want=%d", row.maximum, got, row.scratch)
		}
	}
	settings := DefaultUdpBufferSettingsWithBufferSize(1)
	settings.Log = NewNoopLogger()
	claim := natUdpFlowMemoryByteCount(settings)
	settings.MemoryBudget = NewTransferMemoryBudget(claim - 1)
	newSequence := func() *UdpSequence {
		return NewUdpSequence(t.Context(), nil, SourceId(NewId()), protocol.ProvideMode_Network, 4,
			net.IPv4(192, 0, 2, 1).To4(), 42000, net.IPv4(192, 0, 2, 2).To4(), 443, settings)
	}
	if sequence := newSequence(); sequence != nil {
		sequence.sharedSocketLifecycle = true
		sequence.Close()
		t.Fatal("short flow budget admitted UDP scratch")
	}
	if settings.MemoryBudget.UsedByteCount() != 0 {
		t.Fatal("refused flow retained budget")
	}
	settings.MemoryBudget.SetTotalByteCount(claim)
	sequence := newSequence()
	if sequence == nil {
		t.Fatal("exact prepaid flow claim was refused")
	}
	sequence.sharedSocketLifecycle = true
	defer sequence.Close()
	if settings.MemoryBudget.UsedByteCount() != claim {
		t.Fatal("constructor did not own its complete scratch claim")
	}
	sequence.Close()
	if settings.MemoryBudget.UsedByteCount() != 0 {
		t.Fatal("closed unused flow retained scratch")
	}
	// A caller's enormous raw size must not overflow the bounded read's
	// packetization claim, even when admission has an explicit parent.
	settings.ReadBufferByteCount = 65527
	maximumClaim := natUdpFlowMemoryByteCount(settings)
	settings.ReadBufferByteCount = int(^uint(0) >> 1)
	if got := natUdpFlowMemoryByteCount(settings); got != maximumClaim || got < 65528+16*1024 {
		t.Fatalf("extreme read claim=%d want bounded=%d", got, maximumClaim)
	}
	settings.MemoryBudget = NewTransferMemoryBudget(maximumClaim)
	extreme := newSequence()
	if extreme == nil {
		t.Fatal("bounded extreme-setting constructor was refused")
	}
	extreme.sharedSocketLifecycle = true
	defer extreme.Close()
	if settings.MemoryBudget.UsedByteCount() != maximumClaim {
		t.Fatal("extreme-setting scratch claim escaped its parent")
	}
	extreme.Close()
	if settings.MemoryBudget.UsedByteCount() != 0 {
		t.Fatal("extreme-setting flow retained budget")
	}
}
