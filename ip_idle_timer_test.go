// A flow's idle timer is armed once and re-armed only when it fires: a send
// moves the flow's last activity, never its timer, and a fire before the idle
// timeout since that activity re-arms to it. A close is never early. Every
// event runs in virtual time over an in-memory upstream.
package connect

import (
	"io"
	"math"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Moves a pure ACK through the established fast path, which updates the flow
// without waking its send loop.
func (self *tcpReapTestFlow) pureAck() {
	self.t.Helper()
	packet := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions)
	self.stateLock.Lock()
	ackNumber := self.providerSeq
	self.stateLock.Unlock()
	if !self.sequence.applyEstablishedPureAck(self.source, TransferKey{}, &parsedTcp{
		ack:        true,
		seq:        self.sourceSeq,
		ackNumber:  ackNumber,
		windowSize: math.MaxUint16,
	}, packet) {
		MessagePoolReturn(packet)
		self.t.Fatal("the established flow refused a pure ACK")
	}
}

// A TCP flow's sends do not touch its idle timer, and the flow is still
// reaped exactly IdleTimeout after the last one.
func TestTcpFlowIdleTimerIsNotReArmedPerSend(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		// only the idle source can end the flow
		settings.ReadTimeout = time.Hour
		idleTimeout := settings.IdleTimeout
		arms := atomic.Int64{}
		settings.afterIdleTimerArmForTest = func() {
			arms.Add(1)
		}
		f := newTcpReapTestFlow(t, settings)
		go io.Copy(io.Discard, f.upstream)

		before := arms.Load()
		const sendCount = 64
		var lastSendAt time.Time
		for index := range sendCount {
			time.Sleep(100 * time.Millisecond)
			f.send(f.newSendItem([]byte{byte(index)}))
			lastSendAt = time.Now()
			synctest.Wait()
		}
		if got := arms.Load() - before; got != 0 {
			t.Errorf("%d sends re-armed the idle timer %d times, want none", sendCount, got)
		}

		time.Sleep(lastSendAt.Add(idleTimeout).Sub(time.Now()) - time.Nanosecond)
		synctest.Wait()
		if f.done() {
			t.Fatal("the flow was reaped before IdleTimeout after its last send")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !f.done() {
			t.Fatalf("the flow was not reaped %s after its last send", time.Since(lastSendAt))
		}
	})
}

// A pure ACK never wakes the send loop, yet it is activity: a flow that took
// one inside the idle window is not reaped at the window's end, and is reaped
// a full window later.
func TestTcpFlowIdleCloseWaitsOutPureAckActivity(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		settings.ReadTimeout = time.Hour
		idleTimeout := settings.IdleTimeout
		f := newTcpReapTestFlow(t, settings)
		handshakeAt := time.Now()

		time.Sleep(idleTimeout / 2)
		f.pureAck()
		time.Sleep(idleTimeout / 2)
		synctest.Wait()
		if f.done() {
			t.Fatal("the flow was reaped although a pure ACK arrived within IdleTimeout")
		}
		time.Sleep(idleTimeout - time.Nanosecond)
		synctest.Wait()
		if f.done() {
			t.Fatal("the flow was reaped before a full idle window after the pure ACK's window")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !f.done() {
			t.Fatalf("the flow was not reaped %s after its handshake", time.Since(handshakeAt))
		}
	})
}

// A UDP flow's datagrams do not touch its idle timer, and the flow is still
// reaped exactly IdleTimeout after the last one.
func TestUdpFlowIdleTimerIsNotReArmedPerSend(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultUdpBufferSettingsWithBufferSize(4)
		// only the idle source can end the flow
		settings.ReadTimeout = time.Hour
		idleTimeout := settings.IdleTimeout
		arms := atomic.Int64{}
		settings.afterIdleTimerArmForTest = func() {
			arms.Add(1)
		}
		f := newUdpDeadlineTestFlow(t, settings)
		go io.Copy(io.Discard, f.upstream)

		before := arms.Load()
		const sendCount = 64
		var lastSendAt time.Time
		for index := range sendCount {
			time.Sleep(100 * time.Millisecond)
			f.send([]byte{byte(index)})
			lastSendAt = time.Now()
			synctest.Wait()
		}
		if got := arms.Load() - before; got != 0 {
			t.Errorf("%d datagrams re-armed the idle timer %d times, want none", sendCount, got)
		}

		time.Sleep(lastSendAt.Add(idleTimeout).Sub(time.Now()) - time.Nanosecond)
		synctest.Wait()
		if f.done() {
			t.Fatal("the flow was reaped before IdleTimeout after its last datagram")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !f.done() {
			t.Fatalf("the flow was not reaped %s after its last datagram", time.Since(lastSendAt))
		}
	})
}
