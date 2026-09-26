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
func (self *RemoteUserNatProvider) receiveReliableFrames(source TransferPath, frames []*protocol.Frame, peer Peer) {
	for _, frame := range frames {
		if !receiveDeliveryHasTcp([]*protocol.Frame{frame}) {
			ordinary := peer
			ordinary.delivery = nil
			self.ClientReceive(source, []*protocol.Frame{frame}, ordinary)
			continue
		}
		claim := peer.deliveryReceipt().hold()
		if claim == nil {
			return
		}
		func() {
			secured := false
			defer func() { claim.complete(secured) }()
			memory, admitted := self.startMemoryOperation(providerFrameOperationBytes([]*protocol.Frame{frame}))
			if !admitted {
				// These slots are prepaid in the provider envelope. A full
				// data budget must not block ACK/window control inspection.
				packet, err := ipPacketToProviderBytes(frame)
				if err != nil || len(packet) > packetPoolSize || self.memoryOperations == nil || !self.memoryOperations.start() {
					return
				}
				budget, bytes := self.ingressMemory, natProviderIngressBytes
				if smallNatControlPacket(packet) {
					budget, bytes = self.ingressControlMemory, natProviderControlBytes
				}
				if budget == nil {
					self.memoryOperations.finish()
					return
				}
				memory, admitted = reserveNatMemory(budget, bytes)
				if !admitted {
					self.memoryOperations.finish()
					return
				}
			}
			defer self.finishMemoryOperation(&memory)
			secured = self.clientReceiveAdmitted(source, []*protocol.Frame{frame}, peer)
		}()
	}
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
		attempt = func() (bool, error) {
			return tcp4.sendTransferKey(owner.source, owner.peer.TransferKey, owner.peer.ProvideMode, &tcp, 0, owner.packet, &owner.credit)
		}
	} else {
		protocol, source, destination, transport, valid := parseIpv6(packet)
		if !valid || protocol != ipProtocolNumberTcp || !parseTcpPacket(source, destination, transport, &tcp) {
			owner.operation.complete(false)
			return
		}
		attempt = func() (bool, error) {
			return tcp6.sendTransferKey(owner.source, owner.peer.TransferKey, owner.peer.ProvideMode, &tcp, 0, owner.packet, &owner.credit)
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
		accepted, err := attempt()
		if accepted {
			owner.packet = nil
			return receiveDeliverySecured
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
	default:
		owner.operation.retry(owner.provider.localUserNat.reliableCapacity.subscribe, finalAttempt)
	}
}
