package connect

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
)

// Only an internal provider return consumer implements this two-phase API.
// commit consumes every packet, abort consumes no packet, and both eventually
// call released exactly once. Public borrowed callbacks keep their old API.
type udpReturnReadConsumer interface {
	commit(packets [][]byte)
	abort()
}

type udpReturnReadPrepare func(sequence *UdpSequence, readBytes int, released func()) (udpReturnReadConsumer, bool)

var errUdpReturnReadPaused = errors.New("UDP return read awaits admitted ownership")

// Subscriptions precede the admission check. A release between a failed check
// and disabling readiness must wake the paused registration, not be lost.
type udpReturnReadPause struct {
	wakes [3]<-chan struct{}
}

func (*udpReturnReadPause) Error() string { return errUdpReturnReadPaused.Error() }
func (*udpReturnReadPause) Unwrap() error { return errUdpReturnReadPaused }

// A flow's existing fixed envelope pays one datagram + packetization scratch.
// This lease prevents another read from overwriting that owner while a shared
// provider worker retains it. It adds neither a packet queue nor a goroutine.
type udpReturnReadLease struct {
	sequence     *UdpSequence
	consumer     udpReturnReadConsumer
	mutex        sync.Mutex
	producerDone bool
	consumerDone bool
	finished     bool
}

func (self *UdpSequence) prepareReturnRead(readBytes int) (*udpReturnReadLease, error) {
	if self.prepareReturnReadCallback == nil {
		return nil, nil
	}
	pause := &udpReturnReadPause{}
	if shard := self.returnReadPollShard(); shard != nil {
		// All registrations in a shard share this signal, so its capacity
		// coordinator needs only a fixed number of wait channels, not one
		// goroutine or an unaccounted reflect.Select array per flow.
		pause.wakes[0] = shard.returnReadCapacity.subscribe()
	} else {
		pause.wakes[0] = self.returnReadCapacity.subscribe()
	}
	if self.providerReturnCapacity != nil {
		pause.wakes[1] = self.providerReturnCapacity.subscribe()
	}
	if budget := self.udpBufferSettings.MemoryBudget; budget != nil {
		pause.wakes[2] = budget.CapacityNotify()
	}
	if !self.returnReadPending.CompareAndSwap(false, true) {
		return nil, pause
	}
	if !self.startRetirementOperation() {
		self.returnReadPending.Store(false)
		return nil, context.Canceled
	}
	lease := &udpReturnReadLease{sequence: self}
	consumer, admitted := self.prepareReturnReadCallback(self, readBytes, lease.releaseConsumer)
	if !admitted || consumer == nil {
		// A nil admitted consumer means there is no provider subscriber. It
		// preserves the original dispatcher / public callback path exactly.
		lease.releaseConsumer()
		lease.releaseProducer(false)
		if !admitted {
			return nil, pause
		}
		return nil, nil
	}
	lease.consumer = consumer
	return lease, nil
}

func (lease *udpReturnReadLease) finish(notify bool) {
	lease.mutex.Lock()
	if lease.finished || !lease.producerDone || !lease.consumerDone {
		lease.mutex.Unlock()
		return
	}
	lease.finished = true
	lease.mutex.Unlock()
	lease.sequence.returnReadPending.Store(false)
	lease.sequence.finishRetirementOperation()
	if notify {
		lease.sequence.returnReadCapacity.notify()
		if shard := lease.sequence.returnReadPollShard(); shard != nil {
			shard.returnReadCapacity.notify()
		}
	}
}

// Takes prepared Transfer credit for this flow's next return read. Every flow
// of a provider reads on credit from one budget, and a read refused it keeps
// its place in line: a release reserves the credit for the oldest waiting
// flow before any woken reader runs, and that flow's next read takes it.
// Waking every paused reader to race for a freed credit let the runtime pick
// the flow it carried, and on one processor one flow took every one. A place
// held in another live provider's budget stays; this one reserves without.
func (self *UdpSequence) takeReturnReadCredit(budget *TransferMemoryBudget, bytes ByteCount) (*preparedSendMemory, bool) {
	if budget == nil {
		return nil, true
	}
	if waiter := self.returnReadCredit.Load(); waiter != nil {
		if waiter.budget != budget {
			return prepareSendMemory(budget, bytes)
		}
		select {
		case <-waiter.ready:
		default:
			return nil, false
		}
		self.returnReadCredit.Store(nil)
		if bytes <= waiter.byteCount {
			// Commit trims the credit to the packets actually read.
			return &preparedSendMemory{budget: budget, bytes: waiter.byteCount}, true
		}
		// A larger read than the one that queued gives its grant back.
		waiter.abandon()
	}
	if bytes <= 0 {
		return nil, false
	}
	reserved, waiter := budget.tryReserveOrQueue(bytes)
	if !reserved {
		self.returnReadCredit.Store(waiter)
		return nil, false
	}
	return &preparedSendMemory{budget: budget, bytes: bytes}, true
}

// Gives up a place in line, or the credit already reserved for it, that no
// live provider can serve.
func (self *UdpSequence) releaseUnservedReturnReadCredit(senders []*providerDatagramSender) {
	waiter := self.returnReadCredit.Load()
	if waiter == nil {
		return
	}
	for _, sender := range senders {
		if sender.readCreditBudget() == waiter.budget {
			return
		}
	}
	self.returnReadCredit.Store(nil)
	waiter.abandon()
}

// Once retirement's last operation finishes no read can take this flow's
// place in line again, so it is given up, with any credit reserved for it.
func (self *UdpSequence) releaseReturnReadCreditIfRetired() {
	if self.retirementOperations == nil {
		return
	}
	select {
	case <-self.retirementOperations.Done():
	default:
		return
	}
	if waiter := self.returnReadCredit.Swap(nil); waiter != nil {
		waiter.abandon()
	}
}

func (self *UdpSequence) returnReadPollShard() *udpSocketReadPollShard {
	if self.socketReadPoller == nil || self.socketReadPollShard < 0 ||
		len(self.socketReadPoller.shards) <= self.socketReadPollShard {
		return nil
	}
	return &self.socketReadPoller.shards[self.socketReadPollShard]
}

// The portable reader already owns a per-flow goroutine. Its real UDP socket
// waits for readability without consuming the datagram, then obtains the same
// credit as the shared poller. Quiet sockets therefore do not hoard flight
// memory. Non-syscall test/custom connections retain the bounded pre-read wait.
func (self *UdpSequence) awaitReturnRead(socket net.Conn, readBuffer []byte) (*udpReturnReadLease, error) {
	if self.prepareReturnReadCallback == nil {
		return nil, nil
	}
	readBytes := len(readBuffer)
	if raw, ok := socketRawConn(socket); ok {
		var readErr error
		awaitReadiness := udpSocketPeekRequiresWait
		if err := raw.Read(func(fd uintptr) bool {
			if awaitReadiness {
				awaitReadiness = false
				return false
			}
			readBytes, readErr = peekUdpSocket(SocketHandle(fd), readBuffer)
			return !errors.Is(readErr, syscall.EAGAIN) && !errors.Is(readErr, syscall.EWOULDBLOCK)
		}); err != nil {
			return nil, err
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	for {
		lease, err := self.prepareReturnRead(readBytes)
		var pause *udpReturnReadPause
		if !errors.As(err, &pause) {
			return lease, err
		}
		select {
		case <-self.ctx.Done():
			return nil, self.ctx.Err()
		case <-pause.wakes[0]:
		case <-pause.wakes[1]:
		case <-pause.wakes[2]:
		}
	}
}

func (lease *udpReturnReadLease) releaseConsumer() {
	lease.mutex.Lock()
	lease.consumerDone = true
	lease.mutex.Unlock()
	lease.finish(true)
}

func (lease *udpReturnReadLease) releaseProducer(notify bool) {
	lease.mutex.Lock()
	lease.producerDone = true
	lease.mutex.Unlock()
	lease.finish(notify)
}

func (lease *udpReturnReadLease) abort() {
	if lease == nil {
		return
	}
	defer lease.releaseProducer(true)
	lease.consumer.abort()
}

func (lease *udpReturnReadLease) commit(packets [][]byte) {
	defer lease.releaseProducer(true)
	lease.consumer.commit(packets)
}
