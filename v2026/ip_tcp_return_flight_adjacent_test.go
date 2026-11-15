package connect

import (
	"context"
	"math"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/v2026/protocol"
)

type tcpReturnFlightEvent struct {
	seq     uint32
	payload string
	fin     bool
}

type tcpReturnFlightFixture struct {
	t        *testing.T
	sequence *TcpSequence
	done     chan struct{}
	closeDo  sync.Once
	lock     sync.Mutex
	events   []tcpReturnFlightEvent
}

// Local to this test file: one real replay worker and its ordinary ACK entry.
// The RTT root separately drives Run's socket reader and actual packetization.
func newTcpReturnFlightFixture(t *testing.T, maxPackets int, base uint32, payload []byte, window uint16, hook func(*TcpSequence, tcpReturnFlightEvent)) *tcpReturnFlightFixture {
	t.Helper()
	return newTcpReturnFlightFixtureWithMtu(t, maxPackets, base, payload, window, 44, hook)
}

func newTcpReturnFlightFixtureWithMtu(t *testing.T, maxPackets int, base uint32, payload []byte, window uint16, mtu int, hook func(*TcpSequence, tcpReturnFlightEvent)) *tcpReturnFlightFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	settings := DefaultTcpBufferSettingsWithBufferSize(16)
	settings.Mtu = mtu
	settings.ReturnRecoveryMaxPacketCount = maxPackets
	settings.MemoryBudget = NewTransferMemoryBudget(2 * 1024 * 1024)
	settings.Log = NewNoopLogger()
	f := &tcpReturnFlightFixture{t: t, done: make(chan struct{})}
	f.sequence = NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		path, payload, err := ParseIpPathWithPayload(packet)
		if err != nil || path == nil || path.Protocol != IpProtocolTcp || !path.Ack || path.Syn || path.Rst {
			t.Errorf("unexpected recovery packet: %v", err)
			return
		}
		event := tcpReturnFlightEvent{seq: path.SequenceNumber, payload: string(payload), fin: path.Fin}
		f.lock.Lock()
		if len(f.events) >= 64 {
			t.Error("bounded recovery observation overflowed")
		} else {
			f.events = append(f.events, event)
		}
		f.lock.Unlock()
		if hook != nil {
			hook(f.sequence, event)
		}
	}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 2).To4(), 41000, net.IPv4(198, 51, 100, 2).To4(), 443, base-1, settings)
	if f.sequence == nil {
		cancel()
		t.Fatal("replay fixture could not reserve its fixed root claim")
	}
	// Run normally establishes these fields from the real handshake. Adjacent
	// cases isolate recovery after all original bytes have already been emitted.
	f.sequence.established = true
	f.sequence.receiveSeqAck = base
	f.sequence.receiveSeq = base + uint32(len(payload))
	f.sequence.receiveWindowSize = uint32(window)
	f.sequence.receiveWindowEnd = base + uint32(window)
	f.sequence.receiveWindowEndSet = true
	if !f.sequence.retainReturnChunk(payload, base, false) {
		cancel()
		f.sequence.memory.release()
		t.Fatal("replay fixture could not retain its original payload")
	}
	go func() { defer close(f.done); f.sequence.runReturnRecovery() }()
	t.Cleanup(func() { cancel(); f.close() })
	synctest.Wait()
	return f
}

func (self *tcpReturnFlightFixture) close() {
	self.closeDo.Do(func() {
		self.sequence.Cancel()
		<-self.done
		self.sequence.releaseReturnChunks()
		self.sequence.memory.release()
		if used := self.sequence.tcpBufferSettings.MemoryBudget.UsedByteCount(); used != 0 {
			self.t.Errorf("joined recovery retained %d root bytes", used)
		}
	})
}

func acknowledgeTcpReturnFlight(t *testing.T, sequence *TcpSequence, number uint32, window uint16) {
	t.Helper()
	packet := MessagePoolGet(40)
	if !sequence.applyEstablishedPureAck(sequence.source, TransferKey{}, &parsedTcp{
		ack: true, seq: 101, ackNumber: number, windowSize: window,
	}, packet) {
		MessagePoolReturn(packet)
		t.Error("ordinary established-flow ACK was refused")
	}
}

func (self *tcpReturnFlightFixture) ack(number uint32, window uint16) {
	acknowledgeTcpReturnFlight(self.t, self.sequence, number, window)
	synctest.Wait()
}

func (self *tcpReturnFlightFixture) trigger(base uint32, window uint16) {
	for range 3 {
		self.ack(base, window)
	}
}

func (self *tcpReturnFlightFixture) snapshot() []tcpReturnFlightEvent {
	self.lock.Lock()
	defer self.lock.Unlock()
	return append([]tcpReturnFlightEvent(nil), self.events...)
}

func requireTcpReturnFlightCount(t *testing.T, f *tcpReturnFlightFixture, count int) []tcpReturnFlightEvent {
	t.Helper()
	synctest.Wait()
	events := f.snapshot()
	if len(events) != count {
		t.Fatalf("recovery emitted %d packets, want %d: %+v", len(events), count, events)
	}
	return events
}

func TestTcpReturnFlightWaitsForWholeFlightAck(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		f := newTcpReturnFlightFixture(t, 4, base, []byte("abcdefghijklmnopqrstuvwxyz012345"), 128, nil)
		defer f.close()
		f.trigger(base, 128)
		requireTcpReturnFlightCount(t, f, 4)
		f.ack(base+4, 124)
		time.Sleep(100 * time.Millisecond)
		requireTcpReturnFlightCount(t, f, 4)
		f.ack(base+16, 112)
		events := requireTcpReturnFlightCount(t, f, 8)
		for index, event := range events {
			if event.seq != base+uint32(4*index) || len(event.payload) != 4 || event.fin {
				t.Fatalf("closed flight duplicated or skipped a retained range: %+v", events)
			}
		}
		f.ack(base+32, 96)
	})
}

func TestTcpReturnFlightPreservesSilentPeerBackoff(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		f := newTcpReturnFlightFixture(t, 4, base, []byte("abcdefghijklmnopqrstuvwxyz012345"), 128, nil)
		defer f.close()
		f.trigger(base, 128)
		requireTcpReturnFlightCount(t, f, 4)
		delay := f.sequence.tcpBufferSettings.ReturnResendTimeout
		time.Sleep(delay)
		requireTcpReturnFlightCount(t, f, 4)
		time.Sleep(delay)
		events := requireTcpReturnFlightCount(t, f, 5)
		f.sequence.mutex.Lock()
		attempts := f.sequence.returnAttempts
		f.sequence.mutex.Unlock()
		if events[4].seq != base || attempts != 2 {
			t.Fatal("one stalled flight must retry only its oldest packet and increment attempts once")
		}
		time.Sleep(2 * delay)
		requireTcpReturnFlightCount(t, f, 5)
		time.Sleep(2 * delay)
		events = requireTcpReturnFlightCount(t, f, 6)
		f.sequence.mutex.Lock()
		attempts = f.sequence.returnAttempts
		f.sequence.mutex.Unlock()
		if events[5].seq != base || attempts != 3 {
			t.Fatal("silent-peer backoff counted packets rather than flights")
		}
	})
}

func TestTcpReturnFlightKeepsFrozenFrontierAcrossWrap(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		base := uint32(math.MaxUint32 - 5)
		f := newTcpReturnFlightFixture(t, 4, base, []byte("abcdefghijklmnopqrstuvwx"), 128, nil)
		defer f.close()
		f.trigger(base, 128)
		requireTcpReturnFlightCount(t, f, 4)
		f.sequence.mutex.Lock()
		f.sequence.receiveSeq += 4
		f.sequence.mutex.Unlock()
		if !f.sequence.retainReturnChunk([]byte("next"), base+24, false) {
			t.Fatal("new origin bytes were not retained")
		}
		f.ack(base+16, 112)
		events := requireTcpReturnFlightCount(t, f, 6)
		for index, event := range events {
			if event.seq != base+uint32(4*index) {
				t.Fatalf("wrap-safe cursor changed range: %+v", events)
			}
		}
		f.ack(base+24, 104)
		time.Sleep(100 * time.Millisecond)
		requireTcpReturnFlightCount(t, f, 6)
		f.sequence.mutex.Lock()
		retained := f.sequence.returnByteCount
		f.sequence.mutex.Unlock()
		if retained == 0 {
			t.Fatal("the old recovery episode consumed newly issued origin custody")
		}
		time.Sleep(f.sequence.tcpBufferSettings.ReturnResendTimeout - 100*time.Millisecond)
		events = requireTcpReturnFlightCount(t, f, 7)
		if events[6].seq != base+24 || events[6].payload != "next" {
			t.Fatal("a later recovery episode did not acquire the new retained frontier")
		}
		f.ack(base+28, 100)
	})
}

func TestTcpReturnFlightRepairsOneLostMemberWithoutAnotherBurst(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		f := newTcpReturnFlightFixture(t, 4, base, []byte("abcdefghijklmnopqrstuvwxyz012345"), 128, nil)
		defer f.close()
		f.trigger(base, 128)
		requireTcpReturnFlightCount(t, f, 4)
		// The first member arrived, the second was lost, and later already-
		// emitted data supplies duplicate ACKs for that exact missing prefix.
		f.ack(base+4, 124)
		for range 3 {
			f.ack(base+4, 124)
		}
		events := requireTcpReturnFlightCount(t, f, 5)
		if events[4].seq != base+4 || events[4].payload != "efgh" {
			t.Fatal("one missing flight member opened another speculative burst")
		}
		f.ack(base+16, 112)
		events = requireTcpReturnFlightCount(t, f, 9)
		if events[5].seq != base+16 || events[8].seq != base+28 {
			t.Fatal("the ACK covering the repaired flight did not open its next bounded range")
		}
	})
}

func TestTcpReturnFlightAckJumpSkipsPendingPackets(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		f := newTcpReturnFlightFixture(t, 4, base, []byte("abcdefghijklmnopqrstuvwxyz012345"), 128, func(sequence *TcpSequence, event tcpReturnFlightEvent) {
			if event.seq == base {
				// Reentry also proves that the return callback holds no flow mutex.
				acknowledgeTcpReturnFlight(t, sequence, base+12, 116)
			}
		})
		defer f.close()
		f.trigger(base, 128)
		events := requireTcpReturnFlightCount(t, f, 2)
		if events[0].seq != base || events[1].seq != base+12 {
			t.Fatalf("validated ACK jump did not skip queued replay ranges: %+v", events)
		}
	})
}

func TestTcpReturnFlightPmtuShrinkKeepsActualPacketDebit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		payload := make([]byte, 2*4*(1500-40))
		for index := range payload {
			payload[index] = byte(index)
		}
		f := newTcpReturnFlightFixtureWithMtu(t, 4, base, payload, math.MaxUint16, 1500, func(sequence *TcpSequence, event tcpReturnFlightEvent) {
			if event.seq == base {
				// The production PMTU path is atomic and enforces the family
				// minimum. It can change while a replay callback is outside mutex.
				sequence.applyPathMtu(576)
			}
		})
		defer f.close()
		f.trigger(base, math.MaxUint16)
		events := requireTcpReturnFlightCount(t, f, 4)
		end := base
		for index, event := range events {
			want := 576 - 40
			if index == 0 {
				want = 1500 - 40
			}
			if event.seq != end || len(event.payload) != want {
				t.Fatalf("PMTU replay skipped a byte or violated its actual packet debit: %+v", events)
			}
			end += uint32(len(event.payload))
		}
		// Only3068B were offered, although the original byte plan covered5840B.
		// Its real cumulative ACK must immediately open the next bounded flight.
		f.ack(end, math.MaxUint16-uint16(end-base))
		events = requireTcpReturnFlightCount(t, f, 8)
		if events[4].seq != end || len(events[4].payload) != 576-40 {
			t.Fatal("recovery waited for an ACK of bytes never offered after PMTU shrink")
		}
	})
}

func TestTcpReturnFlightValidatedAckClosesPendingWindow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		f := newTcpReturnFlightFixture(t, 4, base, []byte("abcdefghijklmnop"), 16, func(sequence *TcpSequence, event tcpReturnFlightEvent) {
			if event.seq == base {
				// Original copies may arrive while recovery is in its callback.
				// This is a valid ACK of already-emitted bytes, not a raw window edit.
				acknowledgeTcpReturnFlight(t, sequence, base+16, 0)
			}
		})
		defer f.close()
		f.trigger(base, 16)
		requireTcpReturnFlightCount(t, f, 1)
		f.sequence.mutex.Lock()
		closed := f.sequence.receiveSeqAck == base+16 && f.sequence.receiveWindowSize == 0 && f.sequence.returnByteCount == 0
		f.sequence.mutex.Unlock()
		if !closed {
			t.Fatal("validated ACK did not retire the old range at its greatest advertised edge")
		}
	})
}

func TestTcpReturnFlightClipsWindowAndKeepsFinSeparate(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		f := newTcpReturnFlightFixture(t, 4, base, []byte("abcdefgh"), 6, nil)
		defer f.close()
		f.sequence.mutex.Lock()
		f.sequence.receiveSeq++
		f.sequence.mutex.Unlock()
		if !f.sequence.retainReturnChunk(nil, base+8, true) {
			t.Fatal("FIN custody was not retained")
		}
		f.trigger(base, 6)
		events := requireTcpReturnFlightCount(t, f, 2)
		if events[0].payload != "abcd" || events[1].payload != "ef" {
			t.Fatalf("repair exceeded the advertised right edge: %+v", events)
		}
		f.ack(base+6, 2)
		events = requireTcpReturnFlightCount(t, f, 3)
		if events[2].payload != "gh" || events[2].fin {
			t.Fatal("FIN joined the preceding data flight")
		}
		f.ack(base+8, 1)
		events = requireTcpReturnFlightCount(t, f, 4)
		if !events[3].fin || events[3].seq != base+8 || events[3].payload != "" {
			t.Fatal("FIN did not consume exactly its existing sequence-space byte")
		}
		f.ack(base+9, 0)
	})
}

func TestTcpReturnFlightZeroWindowKeepsSingleFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		f := newTcpReturnFlightFixture(t, 4, base, []byte("abcdefghijklmnop"), 0, nil)
		defer f.close()
		f.trigger(base, 0)
		requireTcpReturnFlightCount(t, f, 1)
		// A lost reopen emits no ACK. A silent zero-window receiver retains
		// the existing single-packet fallback rather than opening four slots.
		time.Sleep(2 * f.sequence.tcpBufferSettings.ReturnResendTimeout)
		events := requireTcpReturnFlightCount(t, f, 2)
		for _, event := range events {
			if event.seq != base || len(event.payload) != 4 {
				t.Fatal("silent zero-window fallback expanded into a burst")
			}
		}
	})
}

func TestTcpReturnFlightRefusalKeepsOriginCustody(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		// This callback consumes each replay without an inner ACK, as a
		// downstream refusal/loss after successful Transfer can do.
		f := newTcpReturnFlightFixture(t, 4, base, []byte("abcdefghijklmnop"), 128, nil)
		defer f.close()
		before := f.sequence.tcpBufferSettings.MemoryBudget.UsedByteCount()
		f.trigger(base, 128)
		requireTcpReturnFlightCount(t, f, 4)
		time.Sleep(2 * f.sequence.tcpBufferSettings.ReturnResendTimeout)
		requireTcpReturnFlightCount(t, f, 5)
		f.sequence.mutex.Lock()
		ack, retained := f.sequence.receiveSeqAck, f.sequence.returnByteCount
		f.sequence.mutex.Unlock()
		if ack != base || retained <= 0 || f.sequence.tcpBufferSettings.MemoryBudget.UsedByteCount() != before {
			t.Fatal("callback completion released origin custody or minted a false inner ACK")
		}
	})
}

func TestTcpReturnFlightCancellationStopsPendingWork(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, closeFlow := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			const base = uint32(101)
			f := newTcpReturnFlightFixture(t, 4, base, []byte("abcdefghijklmnop"), 128, func(sequence *TcpSequence, _ tcpReturnFlightEvent) {
				if closeFlow {
					sequence.Close()
				} else {
					sequence.Cancel()
				}
			})
			defer f.close()
			f.trigger(base, 128)
			requireTcpReturnFlightCount(t, f, 1)
			f.close()
		})
	}
}

func TestTcpReturnFlightSettingsAreOwnerLocal(t *testing.T) {
	assertMessagePoolOwnership(t)
	if DefaultTcpBufferSettings().ReturnRecoveryMaxPacketCount != 1 {
		t.Fatal("application default must preserve single-packet recovery")
	}
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		for _, legacy := range []int{0, -1} {
			f := newTcpReturnFlightFixture(t, legacy, base, []byte("abcdefghijklmnop"), 128, nil)
			f.trigger(base, 128)
			requireTcpReturnFlightCount(t, f, 1)
			f.close()
		}
		first := newTcpReturnFlightFixture(t, 1, base, []byte("abcdefghijklmnopqrstuvwxyz012345"), 128, nil)
		second := newTcpReturnFlightFixture(t, 4, base, []byte("abcdefghijklmnopqrstuvwxyz012345"), 128, nil)
		defer first.close()
		defer second.close()
		first.trigger(base, 128)
		second.trigger(base, 128)
		requireTcpReturnFlightCount(t, first, 1)
		requireTcpReturnFlightCount(t, second, 4)
		first.close()
		second.ack(base+16, 112)
		requireTcpReturnFlightCount(t, second, 8)
	})
}

func TestTcpReturnFlightMetadataUsesFlowRootClaim(t *testing.T) {
	settings := DefaultTcpBufferSettingsWithBufferSize(16)
	settings.Mtu = 44
	settings.ReturnRecoveryMaxPacketCount = 4
	settings.MemoryBudget = NewTransferMemoryBudget(mib(2))
	settings = boundedNatTcpMemorySettings(settings, settings.MemoryBudget)
	claim := natTcpFlowMemoryByteCount(settings)
	settings.MemoryBudget = NewTransferMemoryBudget(claim)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	construct := func() *TcpSequence {
		return NewTcpSequence(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
			SourceId(NewId()), protocol.ProvideMode_Network, 4,
			net.IPv4(192, 0, 2, 2).To4(), 41000, net.IPv4(198, 51, 100, 2).To4(), 443, 100, settings)
	}
	first := construct()
	if first == nil {
		t.Fatal("exact root claim did not admit one recovery owner")
	}
	defer first.Cancel()
	defer first.memory.release()
	if second := construct(); second != nil {
		second.Cancel()
		second.memory.release()
		t.Fatal("a second flow bypassed the exact root claim")
	}
	if used := settings.MemoryBudget.UsedByteCount(); used != claim {
		t.Fatalf("flow root claim=%d, retained=%d", claim, used)
	}
	first.memory.release()
	if used := settings.MemoryBudget.UsedByteCount(); used != 0 {
		t.Fatal("flow retirement did not restore its metadata capacity")
	}
	t.Logf("recovery_worker_state=%d bytes; TcpSequence=%d; default remains1", unsafe.Sizeof(tcpReturnRecoveryFlight{}), unsafe.Sizeof(TcpSequence{}))
}
