package connect

import (
	"errors"
	"sync/atomic"
)

// Distinguish ownership transfer to a terminal response from either TCP data
// admission or ordinary capacity pressure. This is used only by the provider's
// reliable ingress path, never by public NAT packet callbacks.
var errReliableIngressResetPending = errors.New("reliable TCP ingress awaits terminal reset acknowledgement")

// A successful Transfer ACK proves that the client accepted the explicit
// reset. It does not claim that the orphan's application bytes were delivered.
// This small response state never borrows the incoming packet or retains its
// provider/lifecycle/receipt. The queue owner observes it from a cancelable
// Waiting operation, so provider shutdown never needs a live peer's ACK.
type providerTerminalResetAckTarget struct {
	result   atomic.Uint32 // 0 pending, 1 ACKed, 2 refused/failed
	wake     chan struct{}
	observer sendAckTarget
}

func (target *providerTerminalResetAckTarget) sendAckResult(bytes ByteCount, err error) {
	defer target.complete(err == nil)
	if target.observer != nil {
		target.observer.sendAckResult(bytes, err)
	}
}

func (target *providerTerminalResetAckTarget) complete(acked bool) {
	result := uint32(2)
	if acked {
		result = 1
	}
	if target.result.CompareAndSwap(0, result) {
		select {
		case target.wake <- struct{}{}:
		default:
		}
	}
}

func (target *providerTerminalResetAckTarget) attempt() receiveDeliveryAttempt {
	switch target.result.Load() {
	case 0:
		return receiveDeliveryWaiting
	case 1:
		return receiveDeliverySecured
	default:
		return receiveDeliveryRejected
	}
}

// Keep the shared NAT worker nonblocking. The existing bounded, policy-checked
// return path either retains this reset until Transfer admission, or rejects
// it and completes the original receipt as failed. Queue admission alone never
// secures the receipt. TCP return options retain Transfer ACK/retry semantics
// and the unchanged ACK lifetime; no synthetic success or new timeout is used.
func (owner *providerReliablePacket) sendTerminalReset(path *IpPath, packet []byte) {
	target := &providerTerminalResetAckTarget{
		wake:     owner.peer.deliveryReceipt().queue.wake,
		observer: owner.provider.returnAckTargetForTest,
	}
	// The final handoff must also publish refusal if policy/callback handling
	// unwinds: the return pipeline's pending-target defer supplies that result.
	defer owner.operation.retry(nil, target.attempt)
	// Contain a failing policy or return callback here, before unwinding can
	// skip the receive operation's attempting-state bookkeeping. The public
	// NAT callback fanout already has this containment; this exceptional
	// direct, outcome-bearing handoff must preserve it as well.
	HandleError(func() {
		owner.provider.receiveTransferWithRecoveryAndRelease(
			owner.source,
			owner.peer.TransferKey,
			owner.peer.ProvideMode,
			receiveRecoveryModeRegenerableControl,
			path,
			packet,
			nil,
			target,
		)
	}, func() { target.complete(false) })
	// All downstream work now borrows only the separate reset packet. There
	// is no remaining reader of the original input, so a canceled wait may
	// release it immediately. A late ACK updates only target, never the dead
	// receipt. retry publishes the wait after any inline result and wakes the
	// owner, preserving completion even if the callback raced this handoff.
}
