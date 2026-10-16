package connect

import (
	"errors"
	"sync/atomic"

	"github.com/urnetwork/connect/protocol"
)

var errReliableIngressNoFlow = errors.New("reliable TCP ingress has no live downstream flow")

// Lazy broadcast generation: no allocation or contended exchange on an
// ordinary dequeue when no reliable owner is waiting for capacity.
type receiveCapacitySignal struct {
	next atomic.Pointer[transferMemoryBudgetNotify]
}

func (s *receiveCapacitySignal) subscribe() <-chan struct{} {
	for {
		if current := s.next.Load(); current != nil {
			return current.channel
		}
		candidate := &transferMemoryBudgetNotify{channel: make(chan struct{})}
		if s.next.CompareAndSwap(nil, candidate) {
			return candidate.channel
		}
	}
}

func (s *receiveCapacitySignal) notify() {
	if s != nil && s.next.Load() != nil {
		if current := s.next.Swap(nil); current != nil {
			close(current.channel)
		}
	}
}

// Reliable TCP takes the same security/parser path under bounded callback
// workspace, but each frame must either secure an owner or fail its receipt.
// A denied security policy is an intentional consume/reset, not congestion.
//
// Decode workspace is transient and shared by every source: the data budget,
// then one prepaid fallback slot per kind. No workspace now is not a failure
// of this frame, whose receipt would cancel this source's whole sequence
// because another source was decoding. The frame is deferred on its receipt
// instead, and this sequence's worker retries it on either workspace's
// capacity edge. Later data of the sequence queues behind deferred data, so
// each flow reaches the NAT in arrival order. A small control never queues:
// as at final admission it may pass pending data, which it can release.
func (self *RemoteUserNatProvider) receiveReliableFrames(source TransferPath, frames []*protocol.Frame, peer Peer) {
	for _, frame := range frames {
		if !receiveDeliveryHasTcp([]*protocol.Frame{frame}) {
			ordinary := peer
			ordinary.delivery = nil
			self.ClientReceive(source, []*protocol.Frame{frame}, ordinary)
			continue
		}
		receipt := peer.deliveryReceipt()
		claim := receipt.hold()
		if claim == nil {
			return
		}
		control := reliableFrameControl(frame)
		if control || !receipt.queue.decodeDeferred() {
			if result := self.receiveReliableFrame(source, frame, peer); result != receiveDeliveryWaiting {
				claim.complete(result == receiveDeliverySecured)
				continue
			}
		}
		// The deferred operation holds its own claim on the receipt.
		claim.complete(self.deferReliableFrame(source, frame, peer, control))
	}
}

func reliableFrameControl(frame *protocol.Frame) bool {
	packet, err := ipPacketToProviderBytes(frame)
	return err == nil && smallNatControlPacket(packet)
}

// Decodes one frame under transient workspace. Waiting means no workspace
// can be had now but some can later; the frame is untouched.
func (self *RemoteUserNatProvider) receiveReliableFrame(source TransferPath, frame *protocol.Frame, peer Peer) receiveDeliveryAttempt {
	bytes := providerFrameOperationBytes([]*protocol.Frame{frame})
	memory, admitted := self.startMemoryOperation(bytes)
	if !admitted {
		// These slots are prepaid in the provider envelope. A full
		// data budget must not block ACK/window control inspection.
		packet, err := ipPacketToProviderBytes(frame)
		if err != nil || self.memoryOperations == nil || !self.memoryOperations.start() {
			return receiveDeliveryRejected
		}
		fallback, fallbackBytes := self.ingressMemory, natProviderIngressBytes
		if smallNatControlPacket(packet) {
			fallback, fallbackBytes = self.ingressControlMemory, natProviderControlBytes
		}
		if fallback == nil || len(packet) > packetPoolSize {
			self.memoryOperations.finish()
			// Only the data budget can hold this frame, once it can ever fit.
			if data := self.memoryBudget(); data == nil || data.TotalByteCount() < bytes {
				return receiveDeliveryRejected
			}
			return receiveDeliveryWaiting
		}
		memory, admitted = reserveNatMemory(fallback, fallbackBytes)
		if !admitted {
			self.memoryOperations.finish()
			return receiveDeliveryWaiting
		}
	}
	defer self.finishMemoryOperation(&memory)
	if self.clientReceiveAdmitted(source, []*protocol.Frame{frame}, peer) {
		return receiveDeliverySecured
	}
	return receiveDeliveryRejected
}

// Defers a frame refused workspace to an operation on its receipt, which owns
// a read-only share of the borrowed frame bytes. The NAT owner its decode
// creates takes a successor claim before the deferred claim completes.
func (self *RemoteUserNatProvider) deferReliableFrame(source TransferPath, frame *protocol.Frame, peer Peer, control bool) bool {
	deferred := &protocol.Frame{MessageType: frame.MessageType, Raw: frame.Raw,
		MessageBytes: MessagePoolShareReadOnly(frame.MessageBytes)}
	fallback := self.ingressMemory
	if control {
		fallback = self.ingressControlMemory
	}
	var subscribe func() <-chan struct{}
	if fallback != nil {
		subscribe = fallback.CapacityNotify
	}
	op := peer.deliveryReceipt().operation(ipPacketFlowKey{}, control, subscribe, func() receiveDeliveryAttempt {
		return self.receiveReliableFrame(source, deferred, peer)
	}, func() {
		MessagePoolReturn(deferred.MessageBytes)
	})
	if op == nil {
		MessagePoolReturn(deferred.MessageBytes)
		return false
	}
	// All sources are subscribed by the sequence worker before attempting.
	op.contexts = []<-chan struct{}{self.ctx.Done()}
	op.budget = self.memoryBudget()
	return true
}

// One packet/claim survives NAT and final TCP queue pressure. No retry
// duplicates the original owner and no downstream WAN ACK is required.
type providerReliablePacket struct {
	provider  *RemoteUserNatProvider
	source    TransferPath
	peer      Peer
	packet    []byte
	memory    natMemoryReservation
	credit    natMemoryReservation
	operation *receiveDeliveryOperation
}

func (self *RemoteUserNatProvider) queueReliablePacket(source TransferPath, peer Peer,
	lifecycle *providerSourceLifecycle, path *IpPath, packet []byte) {
	flow, valid := ipPacketFlowKeyFromPath(path)
	if !valid || !lifecycle.admissions.start() {
		MessagePoolReturn(packet)
		if claim := peer.deliveryReceipt().hold(); claim != nil {
			claim.complete(false)
		}
		return
	}
	owner := &providerReliablePacket{provider: self, source: source, peer: peer, packet: packet}
	owner.operation = peer.deliveryReceipt().operation(flow, smallNatControlPacket(packet),
		self.localUserNat.reliableCapacity.subscribe, owner.enterNat, func() {
			MessagePoolReturn(owner.packet)
			owner.packet = nil
			owner.credit.release()
			owner.memory.release()
			self.releaseSourceLifecycle(source.SourceId, lifecycle)
		})
	if owner.operation == nil {
		MessagePoolReturn(packet)
		self.releaseSourceLifecycle(source.SourceId, lifecycle)
		return
	}
	owner.operation.pipelined = true
	// All sources are subscribed by the sequence worker before attempting.
	owner.operation.contexts = []<-chan struct{}{self.ctx.Done(), self.localUserNat.ctx.Done(), lifecycle.ctx.Done()}
	if budget := self.localUserNat.settings.MemoryBudget; budget != nil {
		owner.operation.budget = budget
	}
}

func (owner *providerReliablePacket) alive() bool {
	return owner.provider.ctx.Err() == nil && owner.provider.localUserNat.ctx.Err() == nil &&
		owner.peer.deliveryReceipt().queue.sequence.ctx.Err() == nil
}

func (owner *providerReliablePacket) enterNat() receiveDeliveryAttempt {
	if !owner.alive() {
		return receiveDeliveryRejected
	}
	nat := owner.provider.localUserNat
	if owner.memory.budget == nil && nat.settings.MemoryBudget != nil {
		budget := nat.settings.MemoryBudget
		if smallNatControlPacket(owner.packet) {
			budget = nat.controlMemory
		}
		bytes := ByteCount(320) + natPacketMemoryByteCount(owner.packet)
		if budget == nil || budget.TotalByteCount() < bytes {
			return receiveDeliveryRejected
		}
		var accepted bool
		owner.memory, accepted = owner.peer.deliveryReceipt().takePacketCredit(budget, bytes)
		if !accepted {
			return receiveDeliveryWaiting
		}
	}
	if !nat.beginSend() {
		return receiveDeliveryRejected
	}
	defer nat.sendWg.Done()
	queued := &SendPacket{source: owner.source, transferKey: owner.peer.TransferKey,
		provideMode: owner.peer.ProvideMode, packets: [][]byte{owner.packet}, reliable: owner}
	packetBytes := ByteCount(len(owner.packet))
	select {
	case nat.sendPackets <- queued:
		owner.provider.packetStatsCounters.recordRemoteIngress(owner.peer.TransportType, 1, packetBytes)
		return receiveDeliveryInFlight
	default:
		return receiveDeliveryWaiting
	}
}

// An already-held Transfer reservation can move atomically within a shared
// root; otherwise admission uses the destination's ordinary exact ceiling.
// The retained receive metadata keeps a separate conservative allowance.
func (r *receiveDeliveryReceipt) takePacketCredit(target *TransferMemoryBudget, bytes ByteCount) (natMemoryReservation, bool) {
	q := r.queue
	q.mutex.Lock()
	defer q.mutex.Unlock()
	item := r.item
	// This is explicit *additional* downstream credit prepaid before SACK,
	// never an inferred slice of the retained receive-root charge.
	if item != nil && item.memoryBudget != nil && bytes <= item.deliveryPrepaid &&
		item.memoryBudget.admissionRoot() == target.admissionRoot() {
		if !item.memoryBudget.tryMoveReservation(target, bytes) {
			return natMemoryReservation{}, false
		}
		item.deliveryPrepaid -= bytes
		item.queueByteCount -= bytes
		return natMemoryReservation{budget: target, bytes: bytes}, true
	}
	return reserveNatMemory(target, bytes)
}

func (owner *providerReliablePacket) enterTcp(tcp4 *Tcp4Buffer, tcp6 *Tcp6Buffer) {
	if !owner.alive() {
		owner.operation.complete(false)
		return
	}
	var tcp parsedTcp
	var attempt func() (bool, error)
	packet := owner.packet
	if packet[0]>>4 == 4 {
		protocol, source, destination, transport, valid := parseIpv4(packet)
		if !valid || protocol != ipProtocolNumberTcp || !parseTcpPacket(source, destination, transport, &tcp) {
			owner.operation.complete(false)
			return
		}
		tcp.ttl = packet[8]
		attempt = func() (bool, error) {
			return tcp4.sendTransferKeyWithOwner(owner.source, owner.peer.TransferKey, owner.peer.ProvideMode, &tcp, 0, owner.packet, &owner.credit, owner)
		}
	} else {
		protocol, source, destination, transport, valid := parseIpv6(packet)
		if !valid || protocol != ipProtocolNumberTcp || !parseTcpPacket(source, destination, transport, &tcp) {
			owner.operation.complete(false)
			return
		}
		tcp.ttl = packet[7]
		attempt = func() (bool, error) {
			return tcp6.sendTransferKeyWithOwner(owner.source, owner.peer.TransferKey, owner.peer.ProvideMode, &tcp, 0, owner.packet, &owner.credit, owner)
		}
	}
	var valid bool
	owner.credit, valid = owner.memory.split(natPacketMemoryByteCount(packet))
	if !valid {
		owner.operation.complete(false)
		return
	}
	finalAttempt := func() receiveDeliveryAttempt {
		if !owner.alive() {
			return receiveDeliveryRejected
		}
		if !owner.operation.previousDataSecured() {
			return receiveDeliveryWaiting
		}
		accepted, err := attempt()
		if accepted {
			owner.packet = nil
			return receiveDeliverySecured
		}
		if err == errReliableIngressResetPending {
			return receiveDeliveryInFlight
		}
		if err != nil {
			owner.traceFinalRejection(&tcp, err)
			return receiveDeliveryRejected
		}
		return receiveDeliveryWaiting
	}
	// Healthy final admission stays on the existing NAT worker. Only actual
	// queue pressure installs a retry edge in the ReceiveSequence slow path.
	switch result := finalAttempt(); result {
	case receiveDeliverySecured:
		owner.operation.complete(true)
	case receiveDeliveryRejected:
		owner.operation.complete(false)
	case receiveDeliveryInFlight:
		// A terminal reset owns asynchronous completion of this receipt.
		return
	default:
		owner.operation.retry(owner.provider.localUserNat.reliableCapacity.subscribe, finalAttempt)
	}
}
