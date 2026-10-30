// The return recovery timer is woken only when its deadline moves earlier.
// Ordinary progress moves it later, which the armed timer finds when it fires,
// so the replay still lands exactly at the last progress plus the delay.
package connect

import (
	"context"
	"math"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type tcpReturnTimerReplay struct {
	at  time.Time
	seq uint32
}

// One flow's return retention and its recovery goroutine, without a socket.
type tcpReturnTimerFixture struct {
	t        *testing.T
	sequence *TcpSequence
	base     uint32
	replays  chan tcpReturnTimerReplay
	wakes    atomic.Int64
}

func newTcpReturnTimerFixture(t *testing.T) *tcpReturnTimerFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultTcpBufferSettingsWithBufferSize(16)
	settings.Log = NewNoopLogger()
	f := &tcpReturnTimerFixture{
		t:       t,
		base:    uint32(101),
		replays: make(chan tcpReturnTimerReplay, 16),
	}
	f.sequence = NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		_, source, destination, transport, valid := parseIpv4(packet)
		var tcp parsedTcp
		if !valid || !parseTcpPacket(source, destination, transport, &tcp) {
			t.Error("invalid replay")
			return
		}
		f.replays <- tcpReturnTimerReplay{at: time.Now(), seq: tcp.seq}
	}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 2).To4(), 41000, net.IPv4(198, 51, 100, 2).To4(), 443, f.base-1, settings)
	if f.sequence == nil {
		t.Fatal("could not reserve a TCP flow")
	}
	f.sequence.afterReturnRecoveryWakeForTest = func() {
		f.wakes.Add(1)
	}
	f.sequence.mutex.Lock()
	f.sequence.receiveSeqAck = f.base
	f.sequence.receiveSeq = f.base
	f.sequence.receiveWindowSize = math.MaxUint16
	f.sequence.mutex.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.sequence.runReturnRecovery()
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		f.sequence.releaseReturnChunks()
		f.sequence.memory.release()
	})
	synctest.Wait()
	return f
}

// Emits and retains the index-th four-byte chunk, as the socket reader does.
func (self *tcpReturnTimerFixture) retain(index int) {
	self.t.Helper()
	start := self.base + uint32(4*index)
	self.sequence.mutex.Lock()
	self.sequence.receiveSeq = start + 4
	self.sequence.mutex.Unlock()
	if !self.sequence.retainReturnChunk([]byte{1, 2, 3, 4}, start, false) {
		self.t.Fatalf("could not retain chunk %d", index)
	}
}

// Acknowledges every chunk before the index-th, as a source's cumulative ACK.
func (self *tcpReturnTimerFixture) ackBefore(index int) {
	self.sequence.mutex.Lock()
	defer self.sequence.mutex.Unlock()
	self.sequence.applySendAckWithLock(&parsedTcp{
		ack:        true,
		ackNumber:  self.base + uint32(4*index),
		windowSize: math.MaxUint16,
	})
}

func (self *tcpReturnTimerFixture) takeReplay() (tcpReturnTimerReplay, bool) {
	synctest.Wait()
	select {
	case replay := <-self.replays:
		return replay, true
	default:
		return tcpReturnTimerReplay{}, false
	}
}

// A streaming flow retains a chunk per read and the source acknowledges each.
// Neither moves the resend deadline earlier, so neither may wake the recovery
// goroutine; the replay of the one unacknowledged chunk still lands exactly
// one resend timeout after the last acknowledgement.
func TestTcpReturnRecoveryTimerSleepsThroughAdvancingAcks(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const chunkCount = 64
		f := newTcpReturnTimerFixture(t)
		resendTimeout := f.sequence.tcpBufferSettings.ReturnResendTimeout
		step := 10 * time.Millisecond
		if resendTimeout <= chunkCount*step {
			t.Fatalf("resend timeout %s cannot outlast the acknowledgements", resendTimeout)
		}

		f.retain(0)
		synctest.Wait()
		armWakes := f.wakes.Load()
		for index := 1; index < chunkCount; index += 1 {
			f.retain(index)
			synctest.Wait()
		}
		if retainWakes := f.wakes.Load() - armWakes; retainWakes != 0 {
			t.Errorf("%d more retained chunks woke the recovery goroutine %d times, want only the first", chunkCount-1, retainWakes)
		}

		before := f.wakes.Load()
		for index := 1; index < chunkCount; index += 1 {
			time.Sleep(step)
			f.ackBefore(index)
			synctest.Wait()
		}
		lastAckAt := time.Now()
		if ackWakes := f.wakes.Load() - before; ackWakes != 0 {
			t.Errorf("%d advancing acknowledgements woke the recovery goroutine %d times, want none", chunkCount-1, ackWakes)
		}
		if replay, ok := f.takeReplay(); ok {
			t.Fatalf("replayed seq %d while every acknowledgement advanced", replay.seq)
		}

		time.Sleep(resendTimeout - time.Nanosecond)
		if replay, ok := f.takeReplay(); ok {
			t.Fatalf("replayed %s after the last acknowledgement, before the resend timeout", replay.at.Sub(lastAckAt))
		}
		time.Sleep(time.Nanosecond)
		replay, ok := f.takeReplay()
		if !ok || !replay.at.Equal(lastAckAt.Add(resendTimeout)) || replay.seq != f.base+4*(chunkCount-1) {
			t.Fatalf("replay ok=%t at %s after the last acknowledgement seq=%d, want %s seq=%d",
				ok, replay.at.Sub(lastAckAt), replay.seq, resendTimeout, f.base+4*(chunkCount-1))
		}
	})
}

// After a timeout replay the next deadline is backed off. An acknowledgement
// that resets the backoff moves the deadline earlier, so it must wake the
// goroutine: the next replay comes one base timeout after that acknowledgement,
// not at the backed-off deadline.
func TestTcpReturnRecoveryAckAfterBackoffWakesTheTimerEarlier(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newTcpReturnTimerFixture(t)
		resendTimeout := f.sequence.tcpBufferSettings.ReturnResendTimeout

		retainedAt := time.Now()
		f.retain(0)
		time.Sleep(resendTimeout)
		replay, ok := f.takeReplay()
		if !ok || !replay.at.Equal(retainedAt.Add(resendTimeout)) || replay.seq != f.base {
			t.Fatalf("first replay ok=%t at %s seq=%d", ok, replay.at.Sub(retainedAt), replay.seq)
		}

		// later data beyond the replayed flight, then an acknowledgement of
		// exactly that flight: recovery ends and the backoff resets
		time.Sleep(resendTimeout / 20)
		f.retain(1)
		time.Sleep(resendTimeout / 20)
		f.ackBefore(1)
		ackAt := time.Now()
		synctest.Wait()

		time.Sleep(resendTimeout - time.Nanosecond)
		if replay, ok := f.takeReplay(); ok {
			t.Fatalf("replayed seq %d %s after the acknowledgement, before the resend timeout", replay.seq, replay.at.Sub(ackAt))
		}
		time.Sleep(time.Nanosecond)
		replay, ok = f.takeReplay()
		if !ok || !replay.at.Equal(ackAt.Add(resendTimeout)) || replay.seq != f.base+4 {
			t.Fatalf("replay after the backoff reset ok=%t at %s after the acknowledgement seq=%d, want %s seq=%d",
				ok, replay.at.Sub(ackAt), replay.seq, resendTimeout, f.base+4)
		}
	})
}

// A partial acknowledgement inside a recovery readies the next hole's replay
// at once, without waiting for any timer.
func TestTcpReturnRecoveryPartialAckReplaysTheNextHoleAtOnce(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newTcpReturnTimerFixture(t)
		resendTimeout := f.sequence.tcpBufferSettings.ReturnResendTimeout

		f.retain(0)
		f.retain(1)
		time.Sleep(resendTimeout)
		if replay, ok := f.takeReplay(); !ok || replay.seq != f.base {
			t.Fatalf("the timeout did not replay the head: ok=%t seq=%d", ok, replay.seq)
		}

		time.Sleep(resendTimeout / 10)
		f.ackBefore(1)
		ackAt := time.Now()
		replay, ok := f.takeReplay()
		if !ok || !replay.at.Equal(ackAt) || replay.seq != f.base+4 {
			t.Fatalf("partial acknowledgement replay ok=%t at %s seq=%d, want at once seq=%d",
				ok, replay.at.Sub(ackAt), replay.seq, f.base+4)
		}
	})
}
