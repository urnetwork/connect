package connect

import (
	"context"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

// Exact scalar/copy evidence from the actual dedicated callback.
type persistControlEvent struct {
	at      time.Time
	mode    receiveRecoveryMode
	seq     uint32
	ack     uint32
	payload string
	valid   bool
}

// Owns one real replay worker; only the pending-reader state is installed by
// the fixture. The separate causal proof exercises its actual Run producer.
type persistControlFixture struct {
	sequence  *TcpSequence
	settings  *TcpBufferSettings
	cancel    context.CancelFunc
	events    chan persistControlEvent
	release   chan struct{}
	completed chan time.Time
	done      chan struct{}
	releaseDo sync.Once
	closeDo   sync.Once
}

// Callback installation and state setup finish before worker start.
func newPersistControlFixture(t *testing.T, version int, base uint32, pending, hold bool, configure func(*TcpBufferSettings)) *persistControlFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultTcpBufferSettingsWithBufferSize(4)
	settings.Log = NewNoopLogger()
	settings.Mtu = 44
	settings.ReturnQueueBudget = NewTransferMemoryBudget(2048)
	if configure != nil {
		configure(settings)
	}
	f := &persistControlFixture{settings: settings, cancel: cancel,
		events: make(chan persistControlEvent, 8), release: make(chan struct{}), completed: make(chan time.Time, 1), done: make(chan struct{})}
	source, destination := net.IPv4(192, 0, 2, 18).To4(), net.IPv4(198, 51, 100, 18).To4()
	if version == 6 {
		settings.Mtu += Ipv6HeaderSize - Ipv4HeaderSizeWithoutExtensions
		source, destination = net.ParseIP("2001:db8::18"), net.ParseIP("2001:db8:1::18")
	}
	var calls atomic.Int64
	f.sequence = newTcpSequenceWithTransferKey(ctx, func(_ TransferPath, _ TransferKey, _ protocol.ProvideMode, mode receiveRecoveryMode, _ *IpPath, packet []byte) {
		_, source, destination, transport, ok := parseIpv4(packet)
		if version == 6 {
			_, source, destination, transport, ok = parseIpv6(packet)
		}
		var tcp parsedTcp
		valid := ok && parseTcpPacket(source, destination, transport, &tcp) && tcp.ack && !tcp.syn && !tcp.fin && !tcp.rst && !tcp.psh
		event := persistControlEvent{at: time.Now(), mode: mode, seq: tcp.seq, ack: tcp.ackNumber, payload: string(tcp.payload), valid: valid}
		select {
		case f.events <- event:
		default:
			t.Error("persist observations exceeded bounded event slots")
		}
		if calls.Add(1) == 1 && hold {
			select {
			case <-f.release:
			case <-ctx.Done():
			}
			f.completed <- time.Now()
		}
	}, SourceId(NewId()), TransferKey{}, protocol.ProvideMode_Network, version,
		source, 41006, destination, 443, base-1, settings)
	f.sequence.receiveSeqAck, f.sequence.receiveSeq = base, base
	f.sequence.receiveWindowSize, f.sequence.receiveWindowEnd = 0, base
	f.sequence.receiveWindowEndSet, f.sequence.handshakeAcked, f.sequence.established = true, true, true
	f.sequence.returnPersistPending = pending
	return f
}

func (self *persistControlFixture) start() {
	go func() { defer close(self.done); self.sequence.runReturnRecovery() }()
}

// Release callbacks before cancel/join, then reconcile the original owners.
func (self *persistControlFixture) close(t *testing.T) {
	t.Helper()
	self.closeDo.Do(func() {
		self.releaseDo.Do(func() { close(self.release) })
		self.cancel()
		<-self.done
		self.sequence.releaseReturnChunks()
		self.sequence.memory.release()
		if used := self.settings.ReturnQueueBudget.UsedByteCount(); used != 0 {
			t.Errorf("joined persist fixture retained %d bytes", used)
		}
	})
}

// The normal pure-ACK transition owns and returns the packet on success.
func (self *persistControlFixture) ack(t *testing.T, number uint32, window uint16) {
	t.Helper()
	packet := MessagePoolGet(40)
	if !self.sequence.applyEstablishedPureAck(self.sequence.source, TransferKey{}, &parsedTcp{ack: true, ackNumber: number, windowSize: window}, packet) {
		MessagePoolReturn(packet)
		t.Fatal("ordinary established ACK/window update was refused")
	}
}

func advancePersistControlClock(d time.Duration) {
	timer := time.NewTimer(d)
	<-timer.C
	synctest.Wait()
}

// Probes have independent control custody and no new TCP sequence-space byte.
func requirePersistControlProbe(t *testing.T, event persistControlEvent, base uint32) {
	t.Helper()
	if !event.valid || event.mode != receiveRecoveryModeDedicatedTcpControl || event.seq != base-1 || event.payload != "\x00" {
		t.Fatalf("not the exact regenerable persist probe: %+v base=%d", event, base)
	}
}

// The pending flag fits the preexisting alignment gap before returnWake.
func TestTcpZeroWindowPersistLayoutAndCustody(t *testing.T) {
	if got := unsafe.Offsetof(TcpSequence{}.returnWake) - unsafe.Offsetof(TcpSequence{}.returnRecoveryEnd); got != 8 {
		t.Fatalf("pending flag changed recovery-state alignment: %d bytes", got)
	}
	t.Logf("TcpSequence=%d TcpBufferSettings=%d chunk=%d pending_flag_offset=%d; settings add two durations, no new perflow queue", unsafe.Sizeof(TcpSequence{}), unsafe.Sizeof(TcpBufferSettings{}), unsafe.Sizeof(tcpReturnChunk{}), unsafe.Offsetof(TcpSequence{}.returnPersistPending))
}

// An idle zero-window flow emits nothing. Only new unsent-reader demand starts
// the clock; this is not a per-connection keepalive policy.
func TestTcpZeroWindowPersistRequiresPendingReader(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPersistControlFixture(t, 4, 101, false, false, nil)
		f.start()
		defer f.close(t)
		advancePersistControlClock(35 * time.Second)
		select {
		case event := <-f.events:
			t.Fatalf("idle zero-window flow emitted a probe: %+v", event)
		default:
		}
		startedAt := time.Now()
		f.sequence.mutex.Lock()
		f.sequence.returnPersistPending = true
		f.sequence.signalReturnRecoveryWithLock()
		f.sequence.mutex.Unlock()
		event := <-f.events
		requirePersistControlProbe(t, event, 101)
		if !event.at.Equal(startedAt.Add(f.settings.ReturnPersistTimeout)) {
			t.Fatal("new reader inherited the idle flow's elapsed clock")
		}
		f.sequence.mutex.Lock()
		unchanged := f.sequence.receiveSeq == 101 && f.sequence.receiveSeqAck == 101 && f.sequence.returnByteCount == 0 && len(f.sequence.returnChunks) == 0 && f.sequence.returnAttempts == 0
		f.sequence.mutex.Unlock()
		if !unchanged {
			t.Fatal("probe created inner TCP custody, stream bytes or ordinary retry credit")
		}
	})
}

// Explicit zero/negative disables do not fall back to a hidden timer.
func TestTcpZeroWindowPersistNonpositiveIntervalDisables(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, interval := range []time.Duration{0, -time.Second} {
			func() {
				f := newPersistControlFixture(t, 4, 101, true, false, func(settings *TcpBufferSettings) { settings.ReturnPersistTimeout = interval })
				f.start()
				defer f.close(t)
				advancePersistControlClock(35 * time.Second)
				select {
				case event := <-f.events:
					t.Fatalf("disabled interval=%s emitted %+v", interval, event)
				default:
				}
			}()
		}
	})
}

// A callback's local admission wait cannot spend the next interval. Backoff
// doubles independently, then remains at the configured cap.
func TestTcpZeroWindowPersistBackoffStartsAfterAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPersistControlFixture(t, 4, 101, true, true, func(settings *TcpBufferSettings) { settings.ReturnPersistMaxTimeout = 3 * time.Second })
		f.start()
		defer f.close(t)
		first := <-f.events
		requirePersistControlProbe(t, first, 101)
		advancePersistControlClock(3 * time.Second)
		f.releaseDo.Do(func() { close(f.release) })
		completedAt := <-f.completed
		synctest.Wait()
		select {
		case event := <-f.events:
			t.Fatalf("local persist admission spent the next retry interval: %+v", event)
		default:
		}
		second, third, fourth := <-f.events, <-f.events, <-f.events
		for _, event := range []persistControlEvent{second, third, fourth} {
			requirePersistControlProbe(t, event, 101)
		}
		if second.at.Sub(completedAt) != 2*time.Second || third.at.Sub(second.at) != 3*time.Second || fourth.at.Sub(third.at) != 3*time.Second {
			t.Fatalf("wrong completion-based backoff: second=%s third=%s fourth=%s", second.at.Sub(completedAt), third.at.Sub(second.at), fourth.at.Sub(third.at))
		}
	})
}

// Caps at or below the base mean a fixed interval, including negative values.
func TestTcpZeroWindowPersistSmallCapKeepsBaseInterval(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, limit := range []time.Duration{0, -time.Second, 500 * time.Millisecond} {
			func() {
				f := newPersistControlFixture(t, 4, 101, true, false, func(settings *TcpBufferSettings) { settings.ReturnPersistMaxTimeout = limit })
				f.start()
				defer f.close(t)
				first, second := <-f.events, <-f.events
				requirePersistControlProbe(t, first, 101)
				requirePersistControlProbe(t, second, 101)
				if second.at.Sub(first.at) != time.Second {
					t.Fatalf("cap=%s changed base interval to %s", limit, second.at.Sub(first.at))
				}
			}()
		}
	})
}

// A window update while the old probe waits supersedes it. A distinct retained
// data head keeps its own original retry clock and ordinary payload custody.
func TestTcpZeroWindowPersistReopenDuringCallbackPreservesNewHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPersistControlFixture(t, 4, 101, true, true, nil)
		f.start()
		defer f.close(t)
		requirePersistControlProbe(t, <-f.events, 101)
		f.ack(t, 101, 4096)
		f.sequence.mutex.Lock()
		f.sequence.receiveSeq = 105
		f.sequence.mutex.Unlock()
		if !f.sequence.retainReturnChunk([]byte("next"), 101, false) {
			t.Fatal("new already-admitted data owner refused")
		}
		advancePersistControlClock(3 * time.Second)
		f.releaseDo.Do(func() { close(f.release) })
		completedAt := <-f.completed
		data := <-f.events
		if !data.valid || data.mode != receiveRecoveryModeTcpSocket || data.seq != 101 || data.payload != "next" || !data.at.Equal(completedAt) {
			t.Fatalf("old probe completion replaced or postponed the new data head: %+v", data)
		}
		f.ack(t, 105, 4096)
		advancePersistControlClock(35 * time.Second)
		select {
		case event := <-f.events:
			t.Fatalf("reopened flow continued probing/replaying: %+v", event)
		default:
		}
	})
}

// Cancellation releases a parked dedicated callback and clears its demand.
func TestTcpZeroWindowPersistCanceledCallbackJoins(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPersistControlFixture(t, 4, 101, true, true, nil)
		f.start()
		defer f.close(t)
		requirePersistControlProbe(t, <-f.events, 101)
		f.cancel()
		<-f.done
		f.sequence.mutex.Lock()
		cleared := !f.sequence.returnPersistPending && f.sequence.returnByteCount == 0 && f.sequence.receiveSeq == 101 && f.sequence.returnAttempts == 0
		f.sequence.mutex.Unlock()
		if !cleared {
			t.Fatal("canceled probe left demand, bytes, or ordinary retry credit")
		}
		select {
		case event := <-f.events:
			t.Fatalf("canceled persist callback started another probe: %+v", event)
		default:
		}
	})
}

// The probe's sequence wraps without creating a byte, and normal negotiated
// window scaling still governs the ACK which disables probing.
func TestTcpZeroWindowPersistIpv6WrapAndScaledReopen(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPersistControlFixture(t, 6, 0, true, false, nil)
		f.sequence.receiveWindowScale = 3
		f.start()
		defer f.close(t)
		event := <-f.events
		requirePersistControlProbe(t, event, 0)
		if event.seq != math.MaxUint32 {
			t.Fatal("probe did not use the already-ACKed wrap byte")
		}
		f.ack(t, 0, 8)
		f.sequence.mutex.Lock()
		valid := f.sequence.receiveWindowSize == 64 && f.sequence.receiveWindowEnd == 64 && !f.sequence.returnPersistPending && f.sequence.receiveSeq == 0 && f.sequence.returnByteCount == 0
		f.sequence.mutex.Unlock()
		if !valid {
			t.Fatal("persist changed ordinary scaled-window or sequence arithmetic")
		}
		advancePersistControlClock(35 * time.Second)
		select {
		case event := <-f.events:
			t.Fatalf("scaled reopen did not stop probing: %+v", event)
		default:
		}
	})
}
