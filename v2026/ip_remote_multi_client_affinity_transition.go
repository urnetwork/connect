// One parent-owned reservation carries a tuple from old delivery authority to
// an admitted replacement. Source callbacks commit it before serialization;
// refusal releases it without modifying the incumbent's connection state.
package connect

import (
	"sync/atomic"
	"time"
)

// The parent lock owns every field except admitted. The reservation is one
// synchronous, deadline-bounded send owner, never a child of the old flow.
// It keeps no packet, channel queue or historical ancestor chain.
type multiClientAffinityTransition struct {
	parent    *RemoteUserNatMultiClient
	previous  *multiClientChannelUpdate
	next      *multiClientChannelUpdate
	deadline  time.Time
	admitted  atomic.Bool
	finished  bool
	retireFor string
}

// An existing callback cannot be overtaken by a new handshake. Receivers never
// wait on renewal: four bounded CAS attempts either acquire delivery custody
// or refuse it. Negative means enrollment is closed, not a held worker lock.
func (self *multiClientChannelUpdate) acquireAffinityDelivery() bool {
	for range 4 {
		owners := self.affinityDelivery.Load()
		if owners < 0 || owners == 1<<62 {
			return false
		}
		if self.affinityDelivery.CompareAndSwap(owners, owners+1) {
			return true
		}
	}
	return false
}

// Every successful acquisition ends at the actual application callback return,
// never at queue admission. A refusal never calls this method.
func (self *multiClientChannelUpdate) releaseAffinityDelivery() {
	self.affinityDelivery.Add(-1)
}

// No callback may be active when a renewal reserves its old generation. A
// successful reservation keeps -1 through commit; only all-refused can reopen.
func (self *multiClientChannelUpdate) closeAffinityDelivery() bool {
	return self.affinityDelivery.CompareAndSwap(0, -1)
}

// One canonical map lookup, shared by the reservation and its invariant tests.
func (self *RemoteUserNatMultiClient) affinityUpdateWithLock(path *IpPath) *multiClientChannelUpdate {
	if path == nil {
		return nil
	}
	switch path.Version {
	case 4:
		return self.ip4PathUpdates[path.ToIp4Path()]
	case 6:
		return self.ip6PathUpdates[path.ToIp6Path()]
	default:
		return nil
	}
}

// Called before any renewal offer. Map retirement and later send lookups
// honor this exact reservation until every candidate's public offer returns.
// The caller must finish it on every path; no receive callback waits for it.
func (self *RemoteUserNatMultiClient) reserveAffinityTransition(
	previous *multiClientChannelUpdate,
	path *IpPath,
	pin flowPin,
	deadline time.Time,
) *multiClientAffinityTransition {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.ctx.Err() != nil || previous == nil || previous.isProbe() ||
		self.affinityUpdateWithLock(path) != previous || self.affinityTransitions[previous] != nil ||
		self.affinityDeliveryActiveWithLock(path) ||
		!time.Now().Before(deadline) || !previous.closeAffinityDelivery() {
		return nil
	}
	previous.stateLock.Lock()
	previous.affinitySources.closed = true
	next := newMultiClientChannelUpdate(self.ctx, path)
	// Renewal starts an ordinary race, including previous healthy candidates.
	// Keep first-answer SYN authority without inheriting historical source bans.
	next.affinitySources.renewal = true
	previous.stateLock.Unlock()
	next.pinned, next.pinAppId = pin.pinned(), pin.appId
	transition := &multiClientAffinityTransition{
		parent: self, previous: previous, next: next, deadline: deadline,
	}
	if self.affinityTransitions == nil {
		self.affinityTransitions = map[*multiClientChannelUpdate]*multiClientAffinityTransition{}
	}
	self.affinityTransitions[previous] = transition
	return transition
}

// Called by both enqueue and dequeue observers, before either can serialize
// the new SYN. All canonical map writers respect the live reservation, so this
// is a guaranteed commit, not a post-enqueue CAS that can decline ownership.
func (self *multiClientAffinityTransition) admit() {
	if self.admitted.Load() {
		return
	}
	parent := self.parent
	parent.stateLock.Lock()
	defer parent.stateLock.Unlock()
	if self.admitted.Load() {
		return
	}
	if self.finished || parent.affinityTransitions[self.previous] != self {
		// An all-refused offer has no accepted source callback. A stale
		// non-owning observer may not reopen its finished reservation.
		return
	}
	// Parent shutdown owns both generations and clears all canonical routes.
	// An accepted source still owns its packet, but cannot reopen that parent.
	if parent.ctx.Err() == nil {
		switch self.next.ipPath.Version {
		case 4:
			parent.ip4PathUpdates[self.next.ipPath.ToIp4Path()] = self.next
		case 6:
			parent.ip6PathUpdates[self.next.ipPath.ToIp6Path()] = self.next
		}
		self.next.activityTime = time.Now()
		if parent.flowUpdates == nil {
			parent.flowUpdates = map[*multiClientChannelUpdate]bool{}
		}
		parent.flowUpdates[self.next] = true
		parent.joinRenewedAffinityWithLock(self.next)
		parent.reliabilityMetrics.flowOpened()
		parent.notifyFlowReaper()
	}
	self.previous.cancel()
	self.admitted.Store(true)
}

// A new generation joins measurement/donor indexes only after real admission.
// It deliberately performs no inheritance: its fresh race creates its origin.
func (self *RemoteUserNatMultiClient) joinRenewedAffinityWithLock(update *multiClientChannelUpdate) {
	var paths []*IpPath
	if self.settings.DestinationAffinity {
		paths = self.affinityIpPathsWithLock(update.ipPath)
	}
	if update.pinAppId != "" {
		paths = []*IpPath{{ServerName: appAffinityName(update.pinAppId)}}
	}
	hardAffinity := update.pinned || self.reliabilitySettings().AffinityMaxTime > 0
	if hardAffinity && self.affinityIp4Paths == nil {
		self.affinityIp4Paths = map[Ip4Path]map[Ip4Path]time.Time{}
	}
	if hardAffinity && self.affinityIp6Paths == nil {
		self.affinityIp6Paths = map[Ip6Path]map[Ip6Path]time.Time{}
	}
	for _, path := range paths {
		switch update.ipPath.Version {
		case 4:
			key := path.ToIp4Path()
			update.affinityIp4Paths[key] = true
			if hardAffinity {
				if self.affinityIp4Paths[key] == nil {
					self.affinityIp4Paths[key] = map[Ip4Path]time.Time{}
				}
				self.affinityIp4Paths[key][update.ipPath.ToIp4Path()] = time.Now()
			}
		case 6:
			key := path.ToIp6Path()
			update.affinityIp6Paths[key] = true
			if hardAffinity {
				if self.affinityIp6Paths[key] == nil {
					self.affinityIp6Paths[key] = map[Ip6Path]time.Time{}
				}
				self.affinityIp6Paths[key][update.ipPath.ToIp6Path()] = time.Now()
			}
		}
	}
	self.bookGroupLedger(false, false, false)
}

// Public offer completion joins every racing candidate before releasing this
// reservation. Exactly one finish can reopen a refused incumbent; no stale
// completion may reopen a committed or subsequently reserved generation.
func (self *multiClientAffinityTransition) finish() {
	parent := self.parent
	parent.stateLock.Lock()
	if self.finished {
		parent.stateLock.Unlock()
		return
	}
	self.finished = true
	if parent.affinityTransitions[self.previous] == self {
		delete(parent.affinityTransitions, self.previous)
	}
	admitted := self.admitted.Load()
	if !admitted {
		self.previous.stateLock.Lock()
		self.previous.affinitySources.closed = false
		self.previous.stateLock.Unlock()
		self.previous.affinityDelivery.CompareAndSwap(-1, 0)
	}
	retireFor := self.retireFor
	parent.stateLock.Unlock()
	if admitted {
		self.previous.Close()
	} else {
		self.next.Close()
		if retireFor == "bench" {
			parent.migrateClientFlows(self.previous.client.Load(), "bench")
		} else if retireFor == "removed" {
			if client := self.previous.client.Load(); client != nil {
				parent.removeClient(client)
			}
		}
	}
	parent.notifyFlowReaper()
}

// Clearing follows commit, never precedes it: a concurrent dequeue observer
// must not see nil while an enqueue observer is still waiting to commit.
func (self *parsedPacketGroup) admitAffinityTransition() {
	if transition := self.affinityTransition.Load(); transition != nil {
		transition.admit()
		self.affinityTransition.CompareAndSwap(transition, nil)
	}
}
