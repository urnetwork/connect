package connect

import (
	"errors"
	"sync/atomic"
)

// Distinguish ownership transfer to a terminal response from either TCP data
// admission or ordinary capacity pressure. This is used only by the provider's
// reliable ingress path, never by public NAT packet callbacks.
var errReliableIngressResetPending = errors.New("reliable TCP ingress awaits terminal reset acknowledgement")

// A live successor superseded this operation's bound, canceled TCP generation.
// This is a local terminal decision; neither bytes nor reset cross generations.
var errReliableIngressObsoleteFlow = errors.New("reliable TCP ingress belongs to a superseded downstream generation")

// A successful Transfer ACK proves that the client accepted the explicit
// reset. Refused capacity on a live return path also settles the regenerable
// input; neither result claims delivery of the orphan's application bytes.
// This small response state never borrows the incoming packet or retains its
// provider/lifecycle/receipt. The queue owner observes it from a cancelable
// Waiting operation, so provider shutdown never needs a live peer's ACK.
type providerTerminalResetAckTarget struct {
	result   atomic.Uint32 // 0 pending, 1 ACKed, 2 refused/failed, 3 refused by return admission
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
	target.finish(result)
}

// The return path had no room for the reset. It stays a regenerable
// response: the peer's next segment on the dead flow elicits another one, so
// the input is settled rather than failed. A failed receipt cancels the whole
// inbound sequence and discards every item it has already acknowledged, and
// return pressure during downloads would otherwise repeat that cancellation
// for every retry of the input.
func (target *providerTerminalResetAckTarget) refuse() {
	target.finish(3)
}

func (target *providerTerminalResetAckTarget) finish(result uint32) {
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
	case 1, 3:
		return receiveDeliverySecured
	default:
		return receiveDeliveryRejected
	}
}

// Capacity refusal settles only a regenerable orphan on a live provider/NAT
// and source. Retirement is a failed receipt, even if the provider still serves
// other sources. Call before releasing the source lease, which can cancel an
// otherwise healthy generation when its last owner leaves.
func (self *RemoteUserNatProvider) refuseTerminalReset(target *providerTerminalResetAckTarget, sourceLifecycle *providerSourceLifecycle) {
	if target == nil {
		return
	}
	if sourceLifecycle != nil && sourceLifecycle.ctx.Err() == nil &&
		self.localUserNat.ctx.Err() == nil && self.returnPathOpen() {
		target.refuse()
	} else {
		target.complete(false)
	}
}

// Only a separately synthesized, payload-free reset may use the exceptional
// disposition. Validate without allocating callback workspace, so malformed
// packets or retained data cannot turn memory refusal into a secured receipt.
func providerTerminalResetPacket(path *IpPath, packet []byte) bool {
	if path == nil || path.Protocol != IpProtocolTcp || len(packet) == 0 {
		return false
	}
	var tcp parsedTcp
	if packet[0]>>4 == 4 {
		protocol, source, destination, transport, valid := parseIpv4(packet)
		return valid && path.Version == 4 && protocol == ipProtocolNumberTcp &&
			parseTcpPacket(source, destination, transport, &tcp) && tcp.rst && len(tcp.payload) == 0
	}
	protocol, source, destination, transport, valid := parseIpv6(packet)
	return valid && path.Version == 6 && protocol == ipProtocolNumberTcp &&
		parseTcpPacket(source, destination, transport, &tcp) && tcp.rst && len(tcp.payload) == 0
}

// Keep the shared NAT worker nonblocking. Capacity refusal on an open return
// path settles this regenerable input; shutdown and policy failures reject it.
// Once Transfer accepts the reset, its real ACK owns completion. Queue admission
// alone never secures the receipt, and the reset keeps its ordinary ACK lifetime.
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
