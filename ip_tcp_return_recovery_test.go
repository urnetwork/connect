package connect

import (
	"bytes"
	"context"
	"math"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Transfer has already returned success at this boundary. The synthetic
// source models a TUN/kernel dropping one inner segment and cumulatively
// acknowledging only contiguous bytes. Every event runs in virtual time.
func checkTcpReturnHole(t *testing.T, initial uint32, dropOffset int, payloadSize int, deadline time.Duration, closeOrigin bool, ipVersion int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	local, origin := net.Pipe()
	settings := DefaultTcpBufferSettingsWithBufferSize(16)
	settings.Log = NewNoopLogger()
	settings.ReadBufferByteCount = 64
	settings.Mtu = 44 // four payload bytes make the exact lost segment visible
	sourceIp, destinationIp := net.IPv4(192, 0, 2, 2).To4(), net.IPv4(198, 51, 100, 2).To4()
	if ipVersion == 6 {
		settings.Mtu += Ipv6HeaderSize - Ipv4HeaderSizeWithoutExtensions
		sourceIp, destinationIp = net.ParseIP("2001:db8::2"), net.ParseIP("2001:db8:1::2")
	}
	settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) { return local, nil }}
	base := initial + 1
	next := base
	pending := map[uint32][]byte{}
	var received []byte
	var callbackMutex sync.Mutex
	dataPackets := 0
	dropped, finReceived := false, false
	var sequence *TcpSequence
	ack := func() {
		packet := MessagePoolGet(40)
		if !sequence.applyEstablishedPureAck(sequence.source, TransferKey{}, &parsedTcp{
			ack: true, seq: base, ackNumber: next, windowSize: math.MaxUint16,
		}, packet) {
			MessagePoolReturn(packet)
		}
	}
	sequence = NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		callbackMutex.Lock()
		defer callbackMutex.Unlock()
		_, source, destination, transport, ok := parseIpv4(packet)
		if ipVersion == 6 {
			_, source, destination, transport, ok = parseIpv6(packet)
		}
		var tcp parsedTcp
		if !ok || !parseTcpPacket(source, destination, transport, &tcp) {
			t.Error("invalid return packet")
			return
		}
		if tcp.syn {
			ack()
			return
		}
		if len(tcp.payload) != 0 {
			dataPackets++
			if !dropped && int(tcp.seq-base) == dropOffset {
				dropped = true
				return // a successful Transfer callback can still lose this segment
			}
			if int32(tcp.seq-next) >= 0 {
				pending[tcp.seq] = bytes.Clone(tcp.payload)
			}
			for {
				payload, ok := pending[next]
				if !ok {
					break
				}
				delete(pending, next)
				received = append(received, payload...)
				next += uint32(len(payload))
			}
			ack()
		}
		if tcp.fin && tcp.seq == next {
			finReceived = true
			next++
			ack()
		}
	}, SourceId(NewId()), protocol.ProvideMode_Network, ipVersion,
		sourceIp, 41000, destinationIp, 443, initial, settings)
	done := make(chan struct{})
	go func() { defer close(done); sequence.Run() }()
	t.Cleanup(func() { cancel(); origin.Close(); <-done })
	packet := MessagePoolGet(40)
	if accepted, err := sequence.send(&TcpSendItem{ipPacket: packet, tcp: parsedTcp{syn: true, seq: initial, windowSize: math.MaxUint16}}, -1); !accepted {
		MessagePoolReturn(packet)
		t.Fatalf("SYN admission: %v", err)
	}
	synctest.Wait()
	want := make([]byte, payloadSize)
	for i := range want {
		want[i] = byte(i + 1)
	}
	if _, err := origin.Write(want); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	if closeOrigin {
		origin.Close()
		synctest.Wait()
	}
	time.Sleep(deadline)
	synctest.Wait()
	if dropOffset >= 0 && !dropped {
		t.Fatal("loss injection did not run")
	}
	if !bytes.Equal(received, want) {
		t.Fatalf("inner TCP hole after successful Transfer delivery: recovered %d/%d bytes by %s", len(received), len(want), deadline)
	}
	if dropOffset < 0 && dataPackets != (payloadSize+3)/4 {
		t.Fatalf("healthy flow emitted %d packets for %d bytes", dataPackets, payloadSize)
	}
	if closeOrigin && !finReceived {
		t.Fatal("origin EOF discarded data or FIN recovery")
	}
}

func TestTcpReturnRecoversLostMiddleAfterTransferDelivery(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { checkTcpReturnHole(t, 100, 4, 32, 100*time.Millisecond, false, 4) })
}

func TestTcpReturnRecoversLostTailWithoutLaterData(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { checkTcpReturnHole(t, 100, 4, 8, 1100*time.Millisecond, false, 4) })
}

func TestTcpReturnRecoveryCrossesSequenceWrap(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { checkTcpReturnHole(t, math.MaxUint32-10, 4, 32, 100*time.Millisecond, false, 4) })
}

func TestTcpReturnKeepsRecoveryAfterOriginEof(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { checkTcpReturnHole(t, 100, 4, 8, 2100*time.Millisecond, true, 4) })
}

func TestTcpReturnHealthyPathDoesNotReplay(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { checkTcpReturnHole(t, 100, -1, 32, 1100*time.Millisecond, false, 4) })
}

func TestTcpReturnRecoveryIpv6(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { checkTcpReturnHole(t, 100, 4, 32, 100*time.Millisecond, false, 6) })
}

func TestTcpReturnCacheSharesAnExactBudgetAcrossFlows(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		budget := NewTransferMemoryBudget(1024)
		newSequence := func() *TcpSequence {
			return &TcpSequence{ctx: ctx, cancel: cancel,
				tcpBufferSettings: &TcpBufferSettings{ReturnQueueBudget: budget},
				returnWake:        make(chan struct{}, 1), returnCapacity: make(chan struct{}, 1),
				ConnectionState: ConnectionState{receiveSeq: 4096, receiveWindowSize: 4096},
			}
		}
		first, second := newSequence(), newSequence()
		defer first.releaseReturnChunks()
		defer second.releaseReturnChunks()
		if !first.retainReturnChunk(make([]byte, 256), 0, false) || !second.retainReturnChunk(make([]byte, 256), 0, false) {
			t.Fatal("initial shared admission failed")
		}
		before := budget.UsedByteCount()
		beforeBuffers := MessagePoolOutstandingByteCount()
		done := make(chan bool, 1)
		go func() { done <- second.retainReturnChunk(make([]byte, 256), 256, false) }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("replay cache exceeded aggregate budget")
		default:
		}
		if budget.UsedByteCount() != before || before > budget.TotalByteCount() {
			t.Fatal("blocked flow reserved beyond the budget")
		}
		if MessagePoolOutstandingByteCount() != beforeBuffers {
			t.Error("blocked retention allocated a replay copy outside the shared budget")
		}
		first.mutex.Lock()
		first.applySendAckWithLock(&parsedTcp{ack: true, ackNumber: 128, windowSize: 4096})
		first.mutex.Unlock()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("partial TCP ACK freed a still-needed chunk")
		default:
		}
		first.mutex.Lock()
		first.applySendAckWithLock(&parsedTcp{ack: true, ackNumber: 256, windowSize: 4096})
		first.mutex.Unlock()
		if !<-done {
			t.Fatal("ACK capacity release did not wake another flow")
		}
		first.releaseReturnChunks()
		second.releaseReturnChunks()
		if budget.UsedByteCount() != 0 {
			t.Fatal("replay teardown leaked shared capacity")
		}
	})
}

func TestTcpReturnCacheCancellationReleasesAWaiter(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		budget := NewTransferMemoryBudget(512)
		sequence := &TcpSequence{ctx: ctx, cancel: cancel,
			tcpBufferSettings: &TcpBufferSettings{ReturnQueueBudget: budget},
			returnWake:        make(chan struct{}, 1), returnCapacity: make(chan struct{}, 1),
		}
		if !sequence.retainReturnChunk(make([]byte, 256), 0, false) {
			t.Fatal("initial admission")
		}
		done := make(chan bool, 1)
		go func() { done <- sequence.retainReturnChunk(make([]byte, 256), 256, false) }()
		synctest.Wait()
		cancel()
		if <-done {
			t.Fatal("canceled waiter retained origin bytes")
		}
		sequence.releaseReturnChunks()
		if budget.UsedByteCount() != 0 {
			t.Fatal("canceled cache leaked capacity")
		}
	})
}

// Packetization assigns sequence space before bounded retention can wait.
// An ACK for that assigned range must not leave an already-delivered chunk
// behind the recovery cursor when admission eventually resumes.
func TestTcpReturnCacheSkipsAlreadyAcknowledgedReadAhead(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget := NewTransferMemoryBudget(1024)
	sequence := &TcpSequence{ctx: ctx, cancel: cancel,
		tcpBufferSettings: &TcpBufferSettings{ReturnQueueBudget: budget},
		ConnectionState:   ConnectionState{receiveSeq: 512, receiveSeqAck: 512},
	}
	defer sequence.releaseReturnChunks()
	if !sequence.retainReturnChunk(make([]byte, 256), 0, false) {
		t.Fatal("already acknowledged bytes should need no admission")
	}
	if budget.UsedByteCount() != 0 || len(sequence.returnChunks) != 0 {
		t.Fatal("read-ahead ACK left a chunk behind the recovery cursor")
	}
}
