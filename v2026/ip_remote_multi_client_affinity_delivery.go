// Application delivery holds one exact-tuple lease through the actual callback,
// including when teardown has already removed the canonical flow map entry.
package connect

// This bounds concurrently executing tuple callbacks, not remembered flows.
// Refusal retains no packet and does not close a shared carrier or wait.
const affinityDeliveryMaxTuples = 1024

// Only the parent lock guards this live callback count. The update pointer is
// held only until the final callback returns; nil denotes unmatched ingress.
type multiClientAffinityDeliveryOwner struct {
	update *multiClientChannelUpdate
	count  uint32
}

// A stack-owned lease is released even when the application callback panics.
// The key owns address values, never slices of borrowed packet storage.
// A nil parent denotes no ownership; non-TCP delivery needs no lease.
type multiClientAffinityDeliveryLease struct {
	parent *RemoteUserNatMultiClient
	update *multiClientChannelUpdate
	key    ipPacketFlowKey
}

// A queued old reset may run after map removal, but never over a newer mapped
// generation. Admission under this lock also prevents creation through that
// absent-map interval while the old callback is actually executing.
func (self *RemoteUserNatMultiClient) acquireAffinityDelivery(
	path *IpPath,
	update *multiClientChannelUpdate,
) (multiClientAffinityDeliveryLease, bool) {
	if path == nil || path.Protocol != IpProtocolTcp {
		return multiClientAffinityDeliveryLease{}, true
	}
	key, valid := ipPacketFlowKeyFromPath(path)
	if !valid {
		return multiClientAffinityDeliveryLease{}, false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.ctx.Err() != nil {
		return multiClientAffinityDeliveryLease{}, false
	}
	if current := self.affinityUpdateWithLock(path); current != nil && current != update {
		return multiClientAffinityDeliveryLease{}, false
	}
	owner, present := self.affinityDeliveries[key]
	if present && (owner.update != update || owner.count == ^uint32(0)) ||
		!present && len(self.affinityDeliveries) == affinityDeliveryMaxTuples {
		return multiClientAffinityDeliveryLease{}, false
	}
	if update != nil && !update.acquireAffinityDelivery() {
		return multiClientAffinityDeliveryLease{}, false
	}
	if self.affinityDeliveries == nil {
		self.affinityDeliveries = map[ipPacketFlowKey]multiClientAffinityDeliveryOwner{}
	}
	owner.update, owner.count = update, owner.count+1
	self.affinityDeliveries[key] = owner
	return multiClientAffinityDeliveryLease{parent: self, update: update, key: key}, true
}

// A lease must be released exactly once, after the actual callback returns.
// Parent shutdown does not clear this map ahead of its executing owners.
func (self multiClientAffinityDeliveryLease) release() {
	if self.parent == nil {
		return
	}
	self.parent.stateLock.Lock()
	defer self.parent.stateLock.Unlock()
	owner := self.parent.affinityDeliveries[self.key]
	if owner.update != self.update || owner.count == 0 {
		panic("affinity delivery lease lost its exact owner")
	}
	if owner.count == 1 {
		delete(self.parent.affinityDeliveries, self.key)
	} else {
		owner.count--
		self.parent.affinityDeliveries[self.key] = owner
	}
	if self.update != nil {
		self.update.releaseAffinityDelivery()
	}
}

// Creation and renewal use this only under the canonical lookup lock. A
// missing flow entry is not proof that its preceding callback has returned.
func (self *RemoteUserNatMultiClient) affinityDeliveryActiveWithLock(path *IpPath) bool {
	if len(self.affinityDeliveries) == 0 || path == nil || path.Protocol != IpProtocolTcp {
		return false
	}
	key, valid := ipPacketFlowKeyFromPath(path)
	return valid && self.affinityDeliveries[key].count != 0
}

// Borrows the packet for the application call. A refused stale generation does
// not consume it; the surrounding receive owner still returns its pooled share.
func (self *RemoteUserNatMultiClient) deliverAffinityReceivePacket(packet *receivePacket) bool {
	lease, admitted := self.acquireAffinityDelivery(packet.IpPath, packet.generation)
	if !admitted {
		return false
	}
	defer lease.release()
	self.deliverReceivePacket(packet.Source, packet.ProvideMode, packet.IpPath, packet.Packet)
	return true
}
