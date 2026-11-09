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

// These tests separate a late inner TCP flight from a lost one at the
// provider return replay worker: a timeout that the original flight answers
// replays once, while a timeout the replay answers keeps acknowledgement-paced
// repair, and a flow's own packets still waiting for Transfer are not lost.

type tcpReturnReplay struct {
	seq   uint32
	bytes int
	tsval uint32
}

type tcpReturnReplayFixture struct {
	sequence *TcpSequence
	settings *TcpBufferSettings
	lock     sync.Mutex
	replays  []tcpReturnReplay
	// Called from the replay worker after each replay is recorded.
	onReplay func(replay tcpReturnReplay)
}

func (self *tcpReturnReplayFixture) snapshot() []tcpReturnReplay {
	self.lock.Lock()
	defer self.lock.Unlock()
	return append([]tcpReturnReplay(nil), self.replays...)
}

func (self *tcpReturnReplayFixture) ack(ack uint32, echo uint32, payload []byte) {
	self.sequence.mutex.Lock()
	defer self.sequence.mutex.Unlock()
	self.sequence.applySendAckWithLock(&parsedTcp{
		ack: true, ackNumber: ack, windowSize: math.MaxUint16,
		enableTimestamp: echo != 0, timestampEcho: echo, payload: payload,
	})
}

func (self *tcpReturnReplayFixture) evidence() tcpReturnEvidence {
	self.sequence.mutex.Lock()
	defer self.sequence.mutex.Unlock()
	return self.sequence.returnEvidence
}

// A 16-byte flight retained at 101 and handed to Transfer. The worker alone
// runs; four-byte replays make each segment visible. Timestamps follow the
// virtual clock in milliseconds, as the production clock does.
func newTcpReturnReplayFixture(t *testing.T, timestamps bool) *tcpReturnReplayFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fixture := &tcpReturnReplayFixture{}
	settings := DefaultTcpBufferSettingsWithBufferSize(16)
	settings.Mtu = 44
	if timestamps {
		settings.Mtu += tcpTimestampOptionByteCount
	}
	settings.Log = NewNoopLogger()
	fixture.settings = settings
	start := time.Now()
	fixture.sequence = NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		_, source, destination, transport, ok := parseIpv4(packet)
		var tcp parsedTcp
		if !ok || !parseTcpPacket(source, destination, transport, &tcp) {
			t.Error("invalid replay packet")
			return
		}
		replay := tcpReturnReplay{seq: tcp.seq, bytes: len(tcp.payload), tsval: tcp.timestampValue}
		fixture.lock.Lock()
		fixture.replays = append(fixture.replays, replay)
		onReplay := fixture.onReplay
		fixture.lock.Unlock()
		if onReplay != nil {
			onReplay(replay)
		}
	}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 2).To4(), 41000, net.IPv4(198, 51, 100, 2).To4(), 443, 100, settings)
	if timestamps {
		fixture.sequence.enableTimestamp = true
		fixture.sequence.timestampValueForTest = func() uint32 {
			return 1000 + uint32(time.Since(start)/time.Millisecond)
		}
	}
	fixture.sequence.receiveSeqAck = 101
	fixture.sequence.receiveSeq = 117
	if !fixture.sequence.retainReturnChunk([]byte("late-flight-data"), 101, false) {
		t.Fatal("flight was not retained")
	}
	done := make(chan struct{})
	go func() { defer close(done); fixture.sequence.runReturnRecovery() }()
	t.Cleanup(func() {
		cancel()
		<-done
		fixture.sequence.releaseReturnChunks()
		fixture.sequence.memory.release()
	})
	return fixture
}

// RFC 3522: the inner round trip outgrew the one-second timer, and the four
// original segments are acknowledged in order after the timer's single replay.
// Each ACK echoes the original segment's timestamp, older than the replay's,
// so the episode ends: no already-delivered segment is replayed again.
func TestTcpReturnLateFlightAfterTimeoutReplaysOnce(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpReturnReplayFixture(t, true)
		originalTsval := fixture.sequence.timestampValue()
		time.Sleep(fixture.settings.ReturnResendTimeout)
		synctest.Wait()
		if replays := fixture.snapshot(); len(replays) != 1 || replays[0].seq != 101 || replays[0].bytes != 4 {
			t.Fatalf("timer replays = %+v, want one replay of 101", replays)
		} else if int32(originalTsval-replays[0].tsval) >= 0 {
			t.Fatalf("replay tsval %d does not follow original %d", replays[0].tsval, originalTsval)
		}
		time.Sleep(500 * time.Millisecond)
		for _, ack := range []uint32{105, 109, 113, 117} {
			fixture.ack(ack, originalTsval, nil)
			synctest.Wait()
			time.Sleep(100 * time.Millisecond)
		}
		if replays := fixture.snapshot(); len(replays) != 1 {
			t.Fatalf("delayed in-order ACKs replayed delivered data: %+v", replays)
		}
		evidence := fixture.evidence()
		if evidence.spuriousTimeoutCount != 1 || evidence.partialReplayCount != 0 || evidence.timeoutReplayCount != 1 {
			t.Fatalf("evidence = %+v, want one undone timeout and no partial replays", evidence)
		}
		fixture.sequence.mutex.Lock()
		active, remaining := fixture.sequence.returnRecoveryActive, len(fixture.sequence.returnChunks)-fixture.sequence.returnHead
		fixture.sequence.mutex.Unlock()
		if active || remaining != 0 {
			t.Fatalf("recovery active=%t remaining chunks=%d after the full flight was acknowledged", active, remaining)
		}
	})
}

// The same timeout when the whole flight was lost: the source's ACK answers
// the replay itself and echoes its timestamp, which proves the loss, so each
// partial ACK repairs the next segment without another timer.
func TestTcpReturnLostFlightWithTimestampsRepairsPerPartialAck(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var fixture *tcpReturnReplayFixture
		fixture = newTcpReturnReplayFixture(t, true)
		fixture.lock.Lock()
		fixture.onReplay = func(replay tcpReturnReplay) {
			go fixture.ack(replay.seq+uint32(replay.bytes), replay.tsval, nil)
		}
		fixture.lock.Unlock()
		time.Sleep(fixture.settings.ReturnResendTimeout)
		synctest.Wait()
		replays := fixture.snapshot()
		if len(replays) != 4 {
			t.Fatalf("lost flight repaired with %d replays before a second timer, want 4: %+v", len(replays), replays)
		}
		for index, replay := range replays {
			if replay.seq != 101+uint32(4*index) || replay.bytes != 4 {
				t.Fatalf("replay %d = %+v, want seq %d", index, replay, 101+4*index)
			}
		}
		evidence := fixture.evidence()
		if evidence.timeoutReplayCount != 1 || evidence.partialReplayCount != 3 || evidence.spuriousTimeoutCount != 0 {
			t.Fatalf("evidence = %+v, want one timeout and three partial replays", evidence)
		}
	})
}

// A flow whose own earlier packets still wait for Transfer has not lost its
// oldest byte. The timer does not replay past that backlog, and it resumes
// one interval after the backlog drains.
func TestTcpReturnTimerWaitsForLocalBacklog(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpReturnReplayFixture(t, true)
		fixture.sequence.returnUnsent.Add(1)
		time.Sleep(2 * fixture.settings.ReturnResendTimeout)
		synctest.Wait()
		if replays := fixture.snapshot(); len(replays) != 0 {
			t.Fatalf("replayed past this flow's unsent backlog: %+v", replays)
		}
		time.Sleep(fixture.settings.ReturnResendTimeout / 2)
		fixture.sequence.returnUnsent.Add(-1)
		time.Sleep(fixture.settings.ReturnResendTimeout / 2)
		synctest.Wait()
		if replays := fixture.snapshot(); len(replays) != 1 || replays[0].seq != 101 {
			t.Fatalf("drained backlog did not restore the timer: %+v", replays)
		}
	})
}

// Duplicate ACKs prove a loss, so a lost fast replay is retried by the
// ordinary backed-off timer even while later packets of the flow are queued.
func TestTcpReturnProvenLossTimerIgnoresLocalBacklog(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpReturnReplayFixture(t, false)
		for range 3 {
			fixture.ack(101, 0, nil)
		}
		synctest.Wait()
		if replays := fixture.snapshot(); len(replays) != 1 {
			t.Fatalf("three duplicate ACKs emitted %d replays, want 1", len(replays))
		}
		fixture.sequence.returnUnsent.Add(1)
		defer fixture.sequence.returnUnsent.Add(-1)
		time.Sleep(2 * fixture.settings.ReturnResendTimeout)
		synctest.Wait()
		if replays := fixture.snapshot(); len(replays) != 2 || replays[1].seq != 101 {
			t.Fatalf("lost fast replay was not retried by its timer: %+v", replays)
		}
	})
}

// RFC 5681: a segment that carries data is not a duplicate acknowledgement,
// even when its acknowledgement number is unchanged.
func TestTcpReturnDataSegmentsAreNotDuplicateAcks(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpReturnReplayFixture(t, false)
		for range 3 {
			fixture.ack(101, 0, []byte("request"))
		}
		synctest.Wait()
		if replays := fixture.snapshot(); len(replays) != 0 {
			t.Fatalf("upload segments triggered a fast replay: %+v", replays)
		}
		for range 3 {
			fixture.ack(101, 0, nil)
		}
		synctest.Wait()
		if replays := fixture.snapshot(); len(replays) != 1 || replays[0].seq != 101 {
			t.Fatalf("three duplicate ACKs emitted %+v, want one replay of 101", replays)
		}
		if evidence := fixture.evidence(); evidence.fastReplayCount != 1 {
			t.Fatalf("evidence = %+v, want one fast replay", evidence)
		}
	})
}

// Oldest-burst-first return scheduling (tcpReturnScheduler).

func newTcpReturnSchedulerForTest(byteCount ByteCount) *tcpReturnScheduler {
	return newTcpReturnScheduler(&TcpBufferSettings{
		ReturnScheduleByteCount:    byteCount,
		ReturnScheduleStallTimeout: time.Second,
		ReturnScheduleAgeTimeout:   4 * time.Second,
		ReturnScheduleAgeByteCount: kib(16),
	})
}

// Reports, and consumes, a pending wake.
func tcpReturnScheduleWoken(flow *tcpReturnScheduleFlow) bool {
	select {
	case <-flow.wake:
		return true
	default:
		return false
	}
}

func tcpReturnScheduleState(scheduler *tcpReturnScheduler) (active int, waiting int) {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	return len(scheduler.active), scheduler.waitingCount
}

// Only the provider profile schedules; the guards always have a value.
func TestTcpReturnScheduleProviderProfile(t *testing.T) {
	if DefaultLocalUserNatSettings().TcpBufferSettings.ReturnScheduleByteCount != 0 {
		t.Fatal("generic NAT profile schedules return data")
	}
	for _, target := range []ByteCount{0, mib(64)} {
		settings := DefaultProviderLocalUserNatSettingsWithMemoryTarget(target)
		scheduler := newTcpReturnScheduler(settings.TcpBufferSettings)
		if scheduler == nil || scheduler.byteCount != defaultProviderTcpReturnScheduleByteCount ||
			scheduler.stallTimeout != defaultTcpReturnScheduleStallTimeout ||
			scheduler.ageTimeout != defaultTcpReturnScheduleAgeTimeout ||
			scheduler.ageByteCount != defaultTcpReturnScheduleAgeByteCount {
			t.Fatalf("provider profile (target %d) scheduler=%+v", target, scheduler)
		}
	}
	scheduler := newTcpReturnScheduler(&TcpBufferSettings{ReturnScheduleByteCount: 1, ReturnScheduleStallTimeout: -1})
	if scheduler.stallTimeout != defaultTcpReturnScheduleStallTimeout ||
		scheduler.ageTimeout != defaultTcpReturnScheduleAgeTimeout ||
		scheduler.ageByteCount != defaultTcpReturnScheduleAgeByteCount {
		t.Fatalf("a guard was disabled: %+v", scheduler)
	}
	var flow *tcpReturnScheduleFlow
	if newTcpReturnScheduler(DefaultTcpBufferSettings()).newFlow() != nil || flow.admit(0, 10) != 10 {
		t.Fatal("an unscheduled flow was limited")
	}
}

// Bursts are served in start order. An older burst is never limited by a
// younger one, a younger one is held before emission while the older bursts
// fill the allowance, and held bursts resume oldest first.
func TestTcpReturnScheduleServesOldestBurstFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scheduler := newTcpReturnSchedulerForTest(kib(64))
		first, second, third := scheduler.newFlow(), scheduler.newFlow(), scheduler.newFlow()
		first.read()
		second.read()
		third.read()
		if grant := first.admit(0, int(kib(100))); grant != int(kib(100)) {
			t.Fatalf("oldest burst granted %d, want its whole window", grant)
		}
		if grant := second.admit(0, int(kib(8))); grant != 0 {
			t.Fatalf("younger burst granted %d behind a full allowance", grant)
		}
		if grant := third.admit(0, int(kib(8))); grant != 0 {
			t.Fatalf("youngest burst granted %d behind a full allowance", grant)
		}
		if active, waiting := tcpReturnScheduleState(scheduler); active != 3 || waiting != 2 {
			t.Fatalf("active=%d waiting=%d", active, waiting)
		}
		// Room for one: only the oldest held burst wakes, and the younger one
		// cannot overtake it before it resumes.
		first.acknowledged(kib(56))
		if !tcpReturnScheduleWoken(second) || tcpReturnScheduleWoken(third) {
			t.Fatal("wake did not follow burst order")
		}
		if grant := third.admit(0, int(kib(8))); grant != 0 {
			t.Fatalf("younger burst overtook a held older burst with %d", grant)
		}
		if grant := second.admit(0, int(kib(16))); grant != int(kib(8)) {
			t.Fatalf("held burst resumed with %d, want the remaining allowance", grant)
		}
		if tcpReturnScheduleWoken(third) {
			t.Fatal("woke a burst with no allowance left")
		}
		if grant := first.admit(kib(56), int(kib(32))); grant != int(kib(32)) {
			t.Fatalf("oldest burst limited to %d by younger bursts", grant)
		}
		// The oldest burst completes; the next follows within the allowance.
		first.acknowledged(0)
		first.drained()
		if active, _ := tcpReturnScheduleState(scheduler); active != 2 {
			t.Fatalf("completed burst still active (%d active)", active)
		}
		if !tcpReturnScheduleWoken(third) {
			t.Fatal("burst end did not wake the next held burst")
		}
		if grant := third.admit(0, int(kib(64))); grant != int(kib(56)) {
			t.Fatalf("next burst granted %d, want the allowance less the older in-flight", grant)
		}
		if grant := second.admit(kib(8), int(kib(64))); grant != int(kib(64)) {
			t.Fatalf("the now oldest burst granted %d", grant)
		}
		second.close()
		third.close()
		if active, waiting := tcpReturnScheduleState(scheduler); active != 0 || waiting != 0 {
			t.Fatalf("closed flows left active=%d waiting=%d", active, waiting)
		}
	})
}

// A client that stops acknowledging cannot hold other clients: its burst
// stops counting once its acknowledgements have not advanced for the stall
// timeout, and a held reader notices on its periodic re-check.
func TestTcpReturnScheduleStalledBurstStopsCounting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		scheduler := newTcpReturnSchedulerForTest(kib(64))
		stalled, younger := scheduler.newFlow(), scheduler.newFlow()
		stalled.read()
		younger.read()
		stalled.admit(0, int(kib(100)))
		if grant := younger.admit(0, int(kib(8))); grant != 0 {
			t.Fatalf("younger burst granted %d", grant)
		}
		time.Sleep(600 * time.Millisecond)
		stalled.acknowledged(kib(96))
		lastProgress := time.Now()
		time.Sleep(600 * time.Millisecond)
		if grant := younger.admit(0, int(kib(8))); grant != 0 {
			t.Fatalf("younger burst granted %d while the older one progresses", grant)
		}
		var timer *time.Timer
		for younger.admit(0, int(kib(8))) == 0 {
			if !younger.wait(ctx, &timer) {
				t.Fatal("held reader canceled")
			}
		}
		if waited := time.Since(lastProgress); waited <= time.Second || time.Second+2*tcpReturnScheduleRecheckInterval < waited {
			t.Fatalf("held burst resumed %v after the last acknowledgement", waited)
		}
		stalled.close()
		younger.close()
	})
}

// A burst held for the age timeout may always keep the age allowance in
// flight, even behind an older burst that keeps progressing.
func TestTcpReturnScheduleAgedBurstKeepsMinimumInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scheduler := newTcpReturnSchedulerForTest(kib(64))
		older, held := scheduler.newFlow(), scheduler.newFlow()
		older.read()
		held.read()
		older.admit(0, int(kib(256)))
		heldAt := time.Now()
		if grant := held.admit(0, int(kib(64))); grant != 0 {
			t.Fatalf("younger burst granted %d", grant)
		}
		for inflight := kib(252); time.Since(heldAt) < 4*time.Second; inflight -= kib(4) {
			time.Sleep(500 * time.Millisecond)
			older.acknowledged(inflight)
			if time.Since(heldAt) < 4*time.Second {
				if grant := held.admit(0, int(kib(64))); grant != 0 {
					t.Fatalf("burst held for %v granted %d before its age", time.Since(heldAt), grant)
				}
			}
		}
		if grant := held.admit(0, int(kib(64))); grant != int(kib(16)) {
			t.Fatalf("aged burst granted %d, want the age allowance", grant)
		}
		if grant := held.admit(kib(16), int(kib(64))); grant != 0 {
			t.Fatalf("aged burst granted %d beyond the age allowance", grant)
		}
		held.acknowledged(kib(4))
		if !tcpReturnScheduleWoken(held) {
			t.Fatal("acknowledgement did not wake the aged burst")
		}
		if grant := held.admit(kib(4), int(kib(64))); grant != int(kib(12)) {
			t.Fatalf("aged burst granted %d, want the age allowance less its in-flight", grant)
		}
		older.close()
		held.close()
	})
}

// A closed flow stops counting at once and wakes the bursts it held; reports
// after close are ignored.
func TestTcpReturnScheduleCloseReleasesHeldBursts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scheduler := newTcpReturnSchedulerForTest(kib(64))
		older, younger := scheduler.newFlow(), scheduler.newFlow()
		older.read()
		younger.read()
		older.admit(0, int(kib(100)))
		if grant := younger.admit(0, int(kib(8))); grant != 0 {
			t.Fatalf("younger burst granted %d", grant)
		}
		older.close()
		if !tcpReturnScheduleWoken(younger) {
			t.Fatal("close did not wake the held burst")
		}
		if grant := younger.admit(0, int(kib(8))); grant != int(kib(8)) {
			t.Fatalf("released burst granted %d", grant)
		}
		older.close()
		older.read()
		older.acknowledged(kib(1))
		older.drained()
		if grant := older.admit(0, int(kib(8))); grant != int(kib(8)) {
			t.Fatalf("closed flow was scheduled (%d)", grant)
		}
		if active, waiting := tcpReturnScheduleState(scheduler); active != 1 || waiting != 0 {
			t.Fatalf("after close active=%d waiting=%d", active, waiting)
		}
		younger.close()
	})
}

// A flow of one provider NAT driven through the real socket reader. The
// synthetic source acknowledges only when the test says so. Each flow has its
// own cancellation, and its cleanup joins Run.
type tcpReturnScheduleTestFlow struct {
	sequence *TcpSequence
	cancel   context.CancelFunc
	origin   net.Conn
	done     chan struct{}
	mutex    sync.Mutex
	next     uint32
	received []byte
	// acknowledge each in-order segment as it arrives
	autoAcknowledge bool
}

func startTcpReturnScheduleTestFlow(t *testing.T, scheduler *tcpReturnScheduler, port uint16) *tcpReturnScheduleTestFlow {
	t.Helper()
	const initial = 1000
	ctx, cancel := context.WithCancel(context.Background())
	local, origin := net.Pipe()
	settings := DefaultTcpBufferSettingsWithBufferSize(16)
	settings.Log = NewNoopLogger()
	settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) { return local, nil }}
	flow := &tcpReturnScheduleTestFlow{cancel: cancel, origin: origin, done: make(chan struct{}), next: initial + 1}
	flow.sequence = NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		_, source, destination, transport, ok := parseIpv4(packet)
		var tcp parsedTcp
		if !ok || !parseTcpPacket(source, destination, transport, &tcp) {
			t.Error("invalid return packet")
			return
		}
		flow.mutex.Lock()
		if len(tcp.payload) != 0 && tcp.seq == flow.next {
			flow.received = append(flow.received, tcp.payload...)
			flow.next += uint32(len(tcp.payload))
		}
		next, autoAcknowledge := flow.next, flow.autoAcknowledge
		flow.mutex.Unlock()
		if autoAcknowledge && len(tcp.payload) != 0 {
			flow.acknowledgeThrough(next)
		}
	}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 2).To4(), port, net.IPv4(198, 51, 100, 2).To4(), 443, initial, settings)
	flow.sequence.returnSchedule = scheduler.newFlow()
	go func() { defer close(flow.done); flow.sequence.Run() }()
	t.Cleanup(func() { cancel(); origin.Close(); <-flow.done })
	packet := MessagePoolGet(40)
	if accepted, err := flow.sequence.send(&TcpSendItem{ipPacket: packet, tcp: parsedTcp{syn: true, seq: initial, windowSize: math.MaxUint16}}, -1); !accepted {
		MessagePoolReturn(packet)
		t.Fatalf("SYN admission: %v", err)
	}
	return flow
}

// Acknowledges everything received so far.
func (self *tcpReturnScheduleTestFlow) acknowledge() {
	self.mutex.Lock()
	next := self.next
	self.mutex.Unlock()
	self.acknowledgeThrough(next)
}

func (self *tcpReturnScheduleTestFlow) acknowledgeThrough(next uint32) {
	packet := MessagePoolGet(40)
	if !self.sequence.applyEstablishedPureAck(self.sequence.source, TransferKey{}, &parsedTcp{
		ack: true, seq: 1001, ackNumber: next, windowSize: math.MaxUint16,
	}, packet) {
		MessagePoolReturn(packet)
	}
}

func (self *tcpReturnScheduleTestFlow) receivedByteCount() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return len(self.received)
}

// Writes from the origin; net.Pipe returns once the reader took every byte.
func (self *tcpReturnScheduleTestFlow) write(t *testing.T, payload []byte) {
	t.Helper()
	go func() {
		if _, err := self.origin.Write(payload); err != nil {
			t.Errorf("origin write: %v", err)
		}
	}()
	synctest.Wait()
}

// Starts an older flow with its burst unacknowledged and a younger flow the
// schedule holds, then checks the younger one took no sequence space.
func startHeldTcpReturnScheduleFlows(t *testing.T, scheduler *tcpReturnScheduler) (older *tcpReturnScheduleTestFlow, younger *tcpReturnScheduleTestFlow) {
	t.Helper()
	older = startTcpReturnScheduleTestFlow(t, scheduler, 41000)
	younger = startTcpReturnScheduleTestFlow(t, scheduler, 41001)
	synctest.Wait()
	older.write(t, make([]byte, kib(48)))
	if older.receivedByteCount() != int(kib(48)) {
		t.Fatalf("oldest burst emitted %d of %d bytes", older.receivedByteCount(), kib(48))
	}
	younger.write(t, bytes.Repeat([]byte{7}, int(kib(4))))
	younger.sequence.mutex.Lock()
	receiveSeq := younger.sequence.receiveSeq
	younger.sequence.mutex.Unlock()
	if younger.receivedByteCount() != 0 || receiveSeq != 1001 {
		t.Fatalf("held flow emitted %d bytes, receiveSeq=%d", younger.receivedByteCount(), receiveSeq)
	}
	if active, waiting := tcpReturnScheduleState(scheduler); active != 2 || waiting != 1 {
		t.Fatalf("active=%d waiting=%d, want the younger flow held", active, waiting)
	}
	return
}

// The younger flow's origin bytes wait before they take inner sequence space
// while the older burst fills the allowance, and the held flow keeps looking
// active to the flow table's LRU eviction. The bytes follow, without any
// recovery replay, once the older burst is acknowledged.
func TestTcpReturnScheduleHoldsYoungerFlowBeforeEmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		scheduler := newTcpReturnSchedulerForTest(kib(16))
		older, younger := startHeldTcpReturnScheduleFlows(t, scheduler)
		// within the stall timeout, so the older burst still counts
		time.Sleep(600 * time.Millisecond)
		if younger.receivedByteCount() != 0 {
			t.Fatal("held flow emitted while the older burst counts")
		}
		if idle := time.Since(younger.sequence.LastActivityTime()); tcpReturnScheduleRecheckInterval < idle {
			t.Fatalf("held flow looks idle for %v to LRU eviction", idle)
		}
		older.acknowledge()
		synctest.Wait()
		younger.mutex.Lock()
		received := bytes.Clone(younger.received)
		younger.mutex.Unlock()
		if !bytes.Equal(received, bytes.Repeat([]byte{7}, int(kib(4)))) {
			t.Fatalf("held flow delivered %d bytes after the older burst ended", len(received))
		}
		younger.sequence.mutex.Lock()
		evidence := younger.sequence.returnEvidence
		younger.sequence.mutex.Unlock()
		if evidence.timeoutReplayCount+evidence.fastReplayCount+evidence.partialReplayCount != 0 {
			t.Fatalf("held data was replayed: %+v", evidence)
		}
		younger.acknowledge()
		synctest.Wait()
		if active, waiting := tcpReturnScheduleState(scheduler); active != 0 || waiting != 0 {
			t.Fatalf("acknowledged bursts left active=%d waiting=%d", active, waiting)
		}
	})
}

// Concurrent downloads through one small allowance all complete intact, with
// no recovery replay, and every hold is released by an event (an
// acknowledgement, a burst end, an older burst resuming) rather than by the
// periodic re-check: synctest.Wait returns before any timer may fire.
func TestTcpReturnScheduleCompletesConcurrentBursts(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		scheduler := newTcpReturnSchedulerForTest(kib(16))
		flows := make([]*tcpReturnScheduleTestFlow, 4)
		for i := range flows {
			flows[i] = startTcpReturnScheduleTestFlow(t, scheduler, uint16(42000+i))
			flows[i].mutex.Lock()
			flows[i].autoAcknowledge = true
			flows[i].mutex.Unlock()
		}
		synctest.Wait()
		for i, flow := range flows {
			go func() {
				if _, err := flow.origin.Write(bytes.Repeat([]byte{byte(i + 1)}, int(kib(192)))); err != nil {
					t.Errorf("origin write: %v", err)
				}
			}()
		}
		synctest.Wait()
		for i, flow := range flows {
			flow.mutex.Lock()
			received := bytes.Clone(flow.received)
			flow.mutex.Unlock()
			if !bytes.Equal(received, bytes.Repeat([]byte{byte(i + 1)}, int(kib(192)))) {
				t.Fatalf("flow %d delivered %d of %d bytes before any timer fired", i, len(received), kib(192))
			}
			flow.sequence.mutex.Lock()
			evidence := flow.sequence.returnEvidence
			flow.sequence.mutex.Unlock()
			if evidence.timeoutReplayCount+evidence.fastReplayCount+evidence.partialReplayCount != 0 {
				t.Fatalf("flow %d replayed: %+v", i, evidence)
			}
		}
		if active, waiting := tcpReturnScheduleState(scheduler); active != 0 || waiting != 0 {
			t.Fatalf("completed bursts left active=%d waiting=%d", active, waiting)
		}
	})
}

// Cancellation releases a reader held by the schedule at once: the held
// flow's workers all exit without any time passing, by its own cancellation
// alone, while the older flow is untouched. Then the older flow's own
// cancellation does the same.
func TestTcpReturnScheduleCancellationReleasesHeldReader(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		scheduler := newTcpReturnSchedulerForTest(kib(16))
		older, younger := startHeldTcpReturnScheduleFlows(t, scheduler)
		start := time.Now()
		younger.cancel()
		synctest.Wait()
		select {
		case <-younger.done:
		default:
			t.Fatal("held flow still running after its cancellation")
		}
		select {
		case <-older.done:
			t.Fatal("canceling the held flow ended the older flow")
		default:
		}
		if active, waiting := tcpReturnScheduleState(scheduler); active != 1 || waiting != 0 {
			t.Fatalf("canceled held flow left active=%d waiting=%d", active, waiting)
		}
		older.cancel()
		synctest.Wait()
		select {
		case <-older.done:
		default:
			t.Fatal("older flow still running after its cancellation")
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("teardown waited %v", elapsed)
		}
		if active, waiting := tcpReturnScheduleState(scheduler); active != 0 || waiting != 0 {
			t.Fatalf("canceled flows left active=%d waiting=%d", active, waiting)
		}
	})
}

// Closing a provider NAT while its schedule holds a flow joins every flow
// worker without waiting on any timer.
func TestLocalUserNatCloseReleasesHeldReturnSchedule(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var dialMutex sync.Mutex
		origins := map[string]net.Conn{}
		settings := DefaultLocalUserNatSettingsWithBufferSize(8)
		settings.Log = NewNoopLogger()
		settings.TcpBufferSettings.ReturnScheduleByteCount = 4096
		settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(_ context.Context, _ string, address string) (net.Conn, error) {
				dialMutex.Lock()
				defer dialMutex.Unlock()
				local, origin := net.Pipe()
				origins[address] = origin
				return local, nil
			},
		}
		nat := NewLocalUserNat(context.Background(), "return schedule close", settings)
		defer func() {
			nat.Close()
			dialMutex.Lock()
			defer dialMutex.Unlock()
			for _, origin := range origins {
				origin.Close()
			}
		}()
		if nat.tcpReturnScheduler == nil {
			t.Fatal("NAT did not install its return scheduler")
		}
		var receiveMutex sync.Mutex
		received := map[uint16]int{}
		defer nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			_, source, destination, transport, ok := parseIpv4(packet)
			var tcp parsedTcp
			if ok && parseTcpPacket(source, destination, transport, &tcp) {
				receiveMutex.Lock()
				received[tcp.destinationPort] += len(tcp.payload)
				receiveMutex.Unlock()
			}
		})()
		source := SourceId(NewId())
		start := func(sourcePort int, destinationPort int, payload int) {
			t.Helper()
			path := &IpPath{
				Version: 4, Protocol: IpProtocolTcp,
				SourceIp: net.IPv4(192, 0, 2, 3).To4(), SourcePort: sourcePort,
				DestinationIp: net.IPv4(198, 51, 100, 7).To4(), DestinationPort: destinationPort,
			}
			packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 1000, nil))
			if !nat.SendPacket(source, protocol.ProvideMode_Network, packet, 0) {
				MessagePoolReturn(packet)
				t.Fatal("SYN refused")
			}
			synctest.Wait()
			dialMutex.Lock()
			origin := origins[path.DestinationHostPort()]
			dialMutex.Unlock()
			if origin == nil {
				t.Fatal("flow did not dial")
			}
			go origin.Write(make([]byte, payload))
			synctest.Wait()
		}
		// The SYN's unscaled 4096-byte window keeps the older burst at the
		// whole allowance; the younger flow is then held before emission.
		start(40001, 443, int(kib(32)))
		start(40002, 444, int(kib(4)))
		receiveMutex.Lock()
		olderBytes, youngerBytes := received[40001], received[40002]
		receiveMutex.Unlock()
		if olderBytes == 0 || youngerBytes != 0 {
			t.Fatalf("older flow emitted %d, held flow %d bytes", olderBytes, youngerBytes)
		}
		if active, waiting := tcpReturnScheduleState(nat.tcpReturnScheduler); active != 2 || waiting != 1 {
			t.Fatalf("active=%d waiting=%d, want one held flow", active, waiting)
		}
		closeStart := time.Now()
		nat.Close()
		synctest.Wait()
		select {
		case <-nat.runDone:
		default:
			t.Fatal("NAT did not finish closing while a flow was held")
		}
		if elapsed := time.Since(closeStart); elapsed != 0 {
			t.Fatalf("NAT close waited %v", elapsed)
		}
		if active, waiting := tcpReturnScheduleState(nat.tcpReturnScheduler); active != 0 || waiting != 0 {
			t.Fatalf("closed NAT left active=%d waiting=%d", active, waiting)
		}
	})
}

// The oldest burst keeps at most the lead bound in flight and resumes as its
// acknowledgements return.
func TestTcpReturnScheduleBoundsLeadInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scheduler := newTcpReturnSchedulerForTest(kib(64))
		oldest := scheduler.newFlow()
		oldest.read()
		if grant := oldest.admit(0, int(mib(1))); grant != int(tcpReturnScheduleLeadByteCount) {
			t.Fatalf("oldest burst granted %d, want the lead bound %d", grant, tcpReturnScheduleLeadByteCount)
		}
		if grant := oldest.admit(tcpReturnScheduleLeadByteCount, int(kib(64))); grant != 0 {
			t.Fatalf("oldest burst granted %d beyond the lead bound", grant)
		}
		oldest.acknowledged(tcpReturnScheduleLeadByteCount - kib(16))
		if !tcpReturnScheduleWoken(oldest) {
			t.Fatal("acknowledgement did not wake the bounded burst")
		}
		if grant := oldest.admit(tcpReturnScheduleLeadByteCount-kib(16), int(kib(64))); grant != int(kib(16)) {
			t.Fatalf("bounded burst resumed with %d, want what was acknowledged", grant)
		}
		oldest.close()
	})
}

// Streams `flow` at the lead bound, acknowledging a quarter of the bound every
// 250 ms so that it keeps counting, until `until` (in virtual time). `each`
// runs after every step. Returns what the flow has in flight.
func streamTcpReturnScheduleBurst(t *testing.T, flow *tcpReturnScheduleFlow, inflight ByteCount, until time.Time, each func()) ByteCount {
	t.Helper()
	for {
		inflight += ByteCount(flow.admit(inflight, int(tcpReturnScheduleLeadByteCount)))
		if !time.Now().Add(250 * time.Millisecond).Before(until) {
			return inflight
		}
		time.Sleep(250 * time.Millisecond)
		inflight = max(0, inflight-tcpReturnScheduleLeadByteCount/4)
		flow.acknowledged(inflight)
		if each != nil {
			each()
		}
	}
}

// A burst becomes a bulk transfer only once it has both run for the bulk
// timeout and sent the bulk byte count. Then, while an interactive burst is
// active, it stops counting against the others, the bursts it held resume at
// once, and it keeps only the bulk trickle in flight. It runs at the lead
// bound again as soon as no interactive burst is active.
func TestTcpReturnScheduleBulkBurstYields(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scheduler := newTcpReturnSchedulerForTest(kib(64))
		bulk, page := scheduler.newFlow(), scheduler.newFlow()
		start := time.Now()
		bulk.read()
		// the page arrives 2 s before the bulk timeout, so the age guard
		// never applies to it
		pageStart := start.Add(tcpReturnScheduleBulkTimeout - 2*time.Second)
		inflight := streamTcpReturnScheduleBurst(t, bulk, 0, pageStart, nil)
		page.read()
		if grant := page.admit(0, int(kib(8))); grant != 0 {
			t.Fatalf("page granted %d behind a streaming burst", grant)
		}
		// sent far more than the bulk byte count, but younger than the bulk
		// timeout: the burst keeps its place
		inflight = streamTcpReturnScheduleBurst(t, bulk, inflight, start.Add(tcpReturnScheduleBulkTimeout), func() {
			if tcpReturnScheduleWoken(page) {
				t.Fatalf("page released %v, before the bulk timeout", time.Since(start))
			}
		})
		time.Sleep(start.Add(tcpReturnScheduleBulkTimeout).Sub(time.Now()))
		// the bulk burst's own next chunk finds it bulk; the page, held
		// without a re-check for longer than the stall timeout, still waits
		if grant := bulk.admit(inflight, int(kib(64))); grant != 0 {
			t.Fatalf("yielding bulk burst granted %d with %d in flight", grant, inflight)
		}
		if !tcpReturnScheduleWoken(page) {
			t.Fatal("the bulk burst did not release the page it held")
		}
		if grant := page.admit(0, int(kib(8))); grant != int(kib(8)) {
			t.Fatalf("page granted %d while the bulk burst yields", grant)
		}
		bulk.acknowledged(0)
		if !tcpReturnScheduleWoken(bulk) {
			t.Fatal("acknowledgement did not wake the yielding bulk burst")
		}
		if grant := bulk.admit(0, int(kib(64))); grant != int(tcpReturnScheduleBulkInFlightByteCount) {
			t.Fatalf("yielding bulk burst granted %d, want the bulk trickle", grant)
		}
		if grant := bulk.admit(tcpReturnScheduleBulkInFlightByteCount, int(kib(64))); grant != 0 {
			t.Fatalf("yielding bulk burst granted %d beyond the bulk trickle", grant)
		}
		// the page completes: no interactive burst is left
		page.acknowledged(0)
		page.drained()
		if !tcpReturnScheduleWoken(bulk) {
			t.Fatal("the end of the interactive burst did not wake the bulk burst")
		}
		if grant := bulk.admit(tcpReturnScheduleBulkInFlightByteCount, int(kib(64))); grant != int(kib(64)) {
			t.Fatalf("bulk burst alone granted %d", grant)
		}
		bulk.close()
		page.close()
		if active, waiting := tcpReturnScheduleState(scheduler); active != 0 || waiting != 0 {
			t.Fatalf("closed flows left active=%d waiting=%d", active, waiting)
		}
	})
}

// A bulk burst yields for at most the bulk yield timeout, even while
// interactive bursts keep arriving; then it takes its place in arrival order
// again, ahead of the younger bursts.
func TestTcpReturnScheduleBulkBurstYieldsForLimitedTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scheduler := newTcpReturnSchedulerForTest(kib(64))
		bulk := scheduler.newFlow()
		start := time.Now()
		bulk.read()
		inflight := streamTcpReturnScheduleBurst(t, bulk, 0, start.Add(tcpReturnScheduleBulkTimeout), nil)
		time.Sleep(start.Add(tcpReturnScheduleBulkTimeout).Sub(time.Now()))
		// a stream of pages, one after another, each acknowledged within the
		// stall timeout
		yieldStart := time.Now()
		pages := 0
		for time.Since(yieldStart) < tcpReturnScheduleBulkYieldTimeout {
			page := scheduler.newFlow()
			page.read()
			if grant := page.admit(0, int(kib(8))); grant != int(kib(8)) {
				t.Fatalf("page %d granted %d after %v of yielding", pages, grant, time.Since(yieldStart))
			}
			inflight = 0
			bulk.acknowledged(inflight)
			if grant := bulk.admit(inflight, int(kib(64))); grant != int(tcpReturnScheduleBulkInFlightByteCount) {
				t.Fatalf("yielding bulk burst granted %d after %v", grant, time.Since(yieldStart))
			}
			inflight = tcpReturnScheduleBulkInFlightByteCount
			time.Sleep(500 * time.Millisecond)
			page.acknowledged(0)
			page.drained()
			page.close()
			pages++
		}
		// the yield timeout has passed with a page in progress: the bulk
		// burst runs at the lead bound and holds the page behind it
		page := scheduler.newFlow()
		page.read()
		bulk.acknowledged(0)
		if grant := bulk.admit(0, int(tcpReturnScheduleLeadByteCount)); grant != int(tcpReturnScheduleLeadByteCount) {
			t.Fatalf("bulk burst granted %d after its yield timeout", grant)
		}
		if grant := page.admit(0, int(kib(8))); grant != 0 {
			t.Fatalf("page granted %d ahead of the bulk burst after its yield timeout", grant)
		}
		if pages < 2 {
			t.Fatalf("only %d pages", pages)
		}
		page.close()
		bulk.close()
		if active, waiting := tcpReturnScheduleState(scheduler); active != 0 || waiting != 0 {
			t.Fatalf("closed flows left active=%d waiting=%d", active, waiting)
		}
	})
}

// A burst that runs past the bulk timeout without sending the bulk byte count
// (a slow response, not a download) keeps its place.
func TestTcpReturnScheduleSlowBurstKeepsItsPlace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scheduler := newTcpReturnSchedulerForTest(kib(64))
		slow, page := scheduler.newFlow(), scheduler.newFlow()
		start := time.Now()
		slow.read()
		inflight := ByteCount(slow.admit(0, int(kib(100))))
		pageStarted := false
		for time.Since(start) < tcpReturnScheduleBulkTimeout+time.Second {
			time.Sleep(500 * time.Millisecond)
			inflight -= kib(4)
			slow.acknowledged(inflight)
			inflight += ByteCount(slow.admit(inflight, int(kib(4))))
			if !pageStarted && tcpReturnScheduleBulkTimeout-2*time.Second <= time.Since(start) {
				// held for less than the age timeout until the loop ends
				page.read()
				pageStarted = true
			}
			if !pageStarted {
				continue
			}
			if grant := page.admit(0, int(kib(8))); grant != 0 || tcpReturnScheduleWoken(page) {
				t.Fatalf("page granted %d at %v behind a slow burst", grant, time.Since(start))
			}
		}
		slow.close()
		page.close()
	})
}

// A download through the real socket reader becomes a bulk transfer and
// yields to a younger response: the response is emitted at once while the
// download trickles, then the download resumes and completes intact, with no
// recovery replay on either flow.
func TestTcpReturnScheduleBulkDownloadYieldsToResponse(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		scheduler := newTcpReturnSchedulerForTest(kib(16))
		download := startTcpReturnScheduleTestFlow(t, scheduler, 43000)
		response := startTcpReturnScheduleTestFlow(t, scheduler, 43001)
		synctest.Wait()
		// the source completes the response flow's handshake, so the response
		// counts as soon as it waits on the schedule
		response.acknowledge()
		synctest.Wait()
		start := time.Now()
		payload := bytes.Repeat([]byte{3}, int(mib(4)))
		written := make(chan struct{})
		go func() {
			defer close(written)
			if _, err := download.origin.Write(payload); err != nil {
				t.Errorf("origin write: %v", err)
			}
		}()
		synctest.Wait()
		// every 250 ms the source acknowledges the download, all of it or all
		// but 32 KiB, which keeps the download over the shared allowance
		step := func(keep uint32) {
			time.Sleep(250 * time.Millisecond)
			download.mutex.Lock()
			next := download.next
			download.mutex.Unlock()
			download.acknowledgeThrough(next - keep)
			synctest.Wait()
		}
		for time.Since(start) < tcpReturnScheduleBulkTimeout-2*time.Second {
			step(uint32(kib(32)))
		}
		if download.receivedByteCount() < int(tcpReturnScheduleBulkByteCount) {
			t.Fatalf("download emitted only %d bytes", download.receivedByteCount())
		}
		response.write(t, bytes.Repeat([]byte{7}, int(kib(16))))
		for time.Since(start) < tcpReturnScheduleBulkTimeout-250*time.Millisecond {
			step(uint32(kib(32)))
			if response.receivedByteCount() != 0 {
				t.Fatalf("response emitted %v, before the download was bulk", time.Since(start))
			}
		}
		// the acknowledgement at the bulk timeout finds the download bulk
		step(uint32(kib(32)))
		if received := response.receivedByteCount(); received != int(kib(16)) {
			t.Fatalf("response emitted %d of %d bytes while the download yields", received, kib(16))
		}
		// the yielding download trickles
		before := download.receivedByteCount()
		for range 3 {
			step(0)
		}
		if trickled := download.receivedByteCount() - before; trickled <= 0 || 3*int(tcpReturnScheduleBulkInFlightByteCount) < trickled {
			t.Fatalf("yielding download emitted %d bytes in three acknowledgements", trickled)
		}
		// the response completes; the download runs at its window again
		response.acknowledge()
		synctest.Wait()
		for download.receivedByteCount() < len(payload) {
			if time.Minute < time.Since(start) {
				t.Fatalf("download emitted %d of %d bytes after %v", download.receivedByteCount(), len(payload), time.Since(start))
			}
			step(0)
		}
		<-written
		download.acknowledge()
		synctest.Wait()
		download.mutex.Lock()
		intact := bytes.Equal(download.received, payload)
		download.mutex.Unlock()
		if !intact {
			t.Fatal("download delivered different bytes")
		}
		for _, flow := range []*tcpReturnScheduleTestFlow{download, response} {
			flow.sequence.mutex.Lock()
			evidence := flow.sequence.returnEvidence
			flow.sequence.mutex.Unlock()
			if evidence.timeoutReplayCount+evidence.fastReplayCount+evidence.partialReplayCount != 0 {
				t.Fatalf("flow replayed: %+v", evidence)
			}
		}
		if active, waiting := tcpReturnScheduleState(scheduler); active != 0 || waiting != 0 {
			t.Fatalf("acknowledged bursts left active=%d waiting=%d", active, waiting)
		}
	})
}
