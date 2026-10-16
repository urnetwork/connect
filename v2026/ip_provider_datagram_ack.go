package connect

import (
	"errors"
	"sync"
)

// Includes the bounded attribution/evidence graph when this is its last
// owner after provider close. This spends existing parent credit; it does not
// increase any parent, queue, or carrier allowance.
const providerDatagramAckMemoryByteCount ByteCount = 2 * 1024

// The detached owner deliberately contains no provider, NAT, source lifecycle,
// socket, context, raw packet or callback closing over one of them. A late ACK
// can only update its independently charged capsule and a small wake channel.
type providerDatagramAck struct {
	handoff                                preparedSendHandoff
	credit                                 *preparedSendMemory
	wake                                   chan struct{}
	mutex                                  sync.Mutex
	written, accepted, terminal, abandoned bool
	err                                    error
	evidence                               *sourceAckEvidence
	observer                               sendAckTarget
	attribution                            *transportPacketAttribution
}

func (ack *providerDatagramAck) preparedSendMemory() *preparedSendMemory { return ack.credit }
func (ack *providerDatagramAck) preparedHandoff() *preparedSendHandoff   { return &ack.handoff }

func providerDatagramUnwrittenRetry(err error) bool {
	return errors.Is(err, errPreparedSendMemoryUnavailable) || errors.Is(err, errSendPackExpiredUnwritten) ||
		errors.Is(err, errPreparedSendCanceled)
}

func (ack *providerDatagramAck) notify() {
	select {
	case ack.wake <- struct{}{}:
	default:
	}
}

func (ack *providerDatagramAck) observeWrite(transport TransportType) {
	ack.mutex.Lock()
	first := !ack.written
	ack.written = true
	ack.mutex.Unlock()
	if first {
		ack.credit.releaseUnusedAfterTransfer()
		if ack.attribution != nil {
			ack.attribution.observe(transport)
		}
		ack.notify()
	}
}

// Admission and terminal callbacks can be inline in either order. Evidence
// begins exactly once only when the caller confirms that Pack took ownership.
func (ack *providerDatagramAck) admitted() {
	ack.mutex.Lock()
	if ack.accepted {
		ack.mutex.Unlock()
		return
	}
	if ack.evidence != nil {
		ack.evidence.admitted(monotonicNanos())
	}
	ack.accepted = true
	terminal, err := ack.terminal, ack.err
	ack.mutex.Unlock()
	if terminal {
		ack.publishAck(err)
	}
}

func (ack *providerDatagramAck) publishAck(err error) {
	if ack.evidence != nil {
		ack.evidence.sendAckResult(0, err)
	}
	if ack.observer != nil {
		ack.observer.sendAckResult(0, err)
	}
}

func (ack *providerDatagramAck) sendAckResult(_ ByteCount, err error) {
	ack.mutex.Lock()
	if ack.terminal {
		ack.mutex.Unlock()
		return
	}
	ack.terminal, ack.err = true, err
	accepted := ack.accepted
	release := ack.abandoned || !providerDatagramUnwrittenRetry(err)
	ack.mutex.Unlock()
	if release {
		ack.credit.release()
	}
	if accepted {
		ack.publishAck(err)
	}
	ack.notify()
}

func (ack *providerDatagramAck) disposition() (written, terminal bool, err error) {
	ack.mutex.Lock()
	defer ack.mutex.Unlock()
	return ack.written, ack.terminal, ack.err
}

// Canceling the upstream actor never releases a credit still owned by an
// accepted Transfer Pack. Its eventual ACK/error releases it, even after this
// provider or socket generation has been destroyed.
func (ack *providerDatagramAck) abandon() {
	ack.mutex.Lock()
	ack.abandoned = true
	release := !ack.accepted || ack.terminal
	ack.mutex.Unlock()
	if release {
		ack.credit.release()
	}
}
