// An ICMP echo flow passes its backend a lazily refreshed deadline, arms its
// idle timer once, and still ends a full timeout after its last read or send.
// Every event runs in virtual time over an in-memory echo backend.
package connect

import (
	"context"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

type icmpTimerTestReply struct {
	sequenceNumber uint16
	payload        []byte
}

// Answers reads from a test-fed channel and honors the read deadline, as the
// socket backend does. Counts the distinct deadlines it is given, which are
// the deadlines the socket backend installs.
type icmpTimerTestEgress struct {
	replies   chan icmpTimerTestReply
	closed    chan struct{}
	closeOnce sync.Once

	stateLock            sync.Mutex
	writeDeadline        time.Time
	writeDeadlineCount   int
	readDeadline         time.Time
	readDeadlineCount    int
	writeSequenceNumbers []uint16
}

func newIcmpTimerTestEgress() *icmpTimerTestEgress {
	return &icmpTimerTestEgress{
		replies: make(chan icmpTimerTestReply),
		closed:  make(chan struct{}),
	}
}

func (self *icmpTimerTestEgress) WriteEcho(deadline time.Time, ttl int, sequenceNumber uint16, payload []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !deadline.Equal(self.writeDeadline) {
		self.writeDeadline = deadline
		self.writeDeadlineCount += 1
	}
	self.writeSequenceNumbers = append(self.writeSequenceNumbers, sequenceNumber)
	return nil
}

func (self *icmpTimerTestEgress) ReadEcho(deadline time.Time) (uint16, []byte, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !deadline.Equal(self.readDeadline) {
			self.readDeadline = deadline
			self.readDeadlineCount += 1
		}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case reply := <-self.replies:
		return reply.sequenceNumber, reply.payload, nil
	case <-timer.C:
		return 0, nil, os.ErrDeadlineExceeded
	case <-self.closed:
		return 0, nil, net.ErrClosed
	}
}

func (self *icmpTimerTestEgress) Close() {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
}

func (self *icmpTimerTestEgress) deadlineCounts() (int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.readDeadlineCount, self.writeDeadlineCount
}

// Runs one echo flow over the in-memory backend inside the caller's synctest
// bubble.
type icmpTimerTestFlow struct {
	t        *testing.T
	egress   *icmpTimerTestEgress
	sequence *IcmpSequence
	runDone  chan struct{}
}

func newIcmpTimerTestFlow(t *testing.T, settings *IcmpBufferSettings) *icmpTimerTestFlow {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &icmpTimerTestFlow{
		t:       t,
		egress:  newIcmpTimerTestEgress(),
		runDone: make(chan struct{}),
	}
	settings.Log = NewNoopLogger()
	settings.newEgressForTest = func(context.Context, int, net.IP, *IcmpBufferSettings) (icmpEgress, error) {
		return f.egress, nil
	}
	f.sequence = NewIcmpSequence(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 3).To4(), 7, net.IPv4(198, 51, 100, 3).To4(), settings)
	if f.sequence == nil {
		t.Fatal("could not admit an echo flow")
	}
	go func() {
		defer close(f.runDone)
		f.sequence.Run()
	}()
	t.Cleanup(func() {
		cancel()
		<-f.runDone
	})
	synctest.Wait()
	return f
}

// Moves one pool-owned echo request's ownership to the flow.
func (self *icmpTimerTestFlow) send(sequenceNumber uint16) {
	self.t.Helper()
	packet := MessagePoolGet(8)
	item := &IcmpSendItem{
		source:      self.sequence.source,
		provideMode: protocol.ProvideMode_Network,
		icmp: parsedIcmp{
			echoRequest:    true,
			sequenceNumber: sequenceNumber,
			ttl:            64,
			payload:        packet,
		},
		ipPacket: packet,
	}
	if accepted, err := self.sequence.send(item, -1); !accepted {
		item.release()
		self.t.Fatalf("the echo flow refused a request: %v", err)
	}
}

func (self *icmpTimerTestFlow) reply(sequenceNumber uint16) {
	self.egress.replies <- icmpTimerTestReply{sequenceNumber: sequenceNumber, payload: []byte{1}}
}

func (self *icmpTimerTestFlow) done() bool {
	select {
	case <-self.runDone:
		return true
	default:
		return false
	}
}

// Echo requests and replies for less than half of each timeout give the
// backend one read and one write deadline, and leave the idle timer alone; the
// flow is reaped exactly IdleTimeout after its last request.
func TestIcmpFlowTimersAreLazy(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultIcmpBufferSettingsWithBufferSize(4)
		// only the idle source can end the flow
		settings.ReadTimeout = time.Hour
		idleTimeout := settings.IdleTimeout
		arms := atomic.Int64{}
		settings.afterIdleTimerArmForTest = func() {
			arms.Add(1)
		}
		f := newIcmpTimerTestFlow(t, settings)

		before := arms.Load()
		const echoCount = 64
		var lastSendAt time.Time
		for index := range echoCount {
			time.Sleep(50 * time.Millisecond)
			f.send(uint16(index))
			lastSendAt = time.Now()
			f.reply(uint16(index))
			synctest.Wait()
		}
		if got := arms.Load() - before; got != 0 {
			t.Errorf("%d echo requests re-armed the idle timer %d times, want none", echoCount, got)
		}
		reads, writes := f.egress.deadlineCounts()
		if 1 < reads || 1 < writes {
			t.Errorf("%d echoes in %s gave the backend %d read and %d write deadlines, want one each",
				echoCount, echoCount*50*time.Millisecond, reads, writes)
		}

		time.Sleep(lastSendAt.Add(idleTimeout).Sub(time.Now()) - time.Nanosecond)
		synctest.Wait()
		if f.done() {
			t.Fatal("the echo flow was reaped before IdleTimeout after its last request")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !f.done() {
			t.Fatalf("the echo flow was not reaped %s after its last request", time.Since(lastSendAt))
		}
	})
}

// A read that began with less than the full timeout left on the deadline is
// not cut short: a destination that stops replying ends the flow a full
// ReadTimeout after the read that followed its last reply began.
func TestIcmpFlowReadStallExpiresAFullTimeoutAfterTheLastReply(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultIcmpBufferSettingsWithBufferSize(4)
		// only the silent destination can end the flow
		settings.IdleTimeout = time.Hour
		readTimeout := settings.ReadTimeout
		f := newIcmpTimerTestFlow(t, settings)

		time.Sleep(readTimeout * 2 / 5)
		f.send(1)
		f.reply(1)
		lastReplyAt := time.Now()
		synctest.Wait()

		time.Sleep(readTimeout - time.Nanosecond)
		synctest.Wait()
		if f.done() {
			t.Fatal("the echo flow ended before ReadTimeout after its last reply")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !f.done() {
			t.Fatalf("the silent destination did not end the flow %s after its last reply", time.Since(lastReplyAt))
		}
	})
}
