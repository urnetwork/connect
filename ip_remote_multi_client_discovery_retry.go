// Expansion pacing for a discovery window.
//
// A discovery window (its specs name locations or the best available exits,
// not client ids) mints a derived platform identity for every candidate it
// evaluates and retires each one it does not admit. When candidates keep
// failing -- the providers do not answer within the setup or ping deadline, or
// the platform is too slow to complete the contract or the connection -- the
// resize loop started the next pass as soon as the last one ended, because a
// failed pass already outlasts WindowResizeTimeout. On Main on 2026-10-21 the
// hosted proxy's discovery windows minted and retired about 340 identities a
// second this way, half of them never connecting, and the load they put on the
// shared database kept the next passes failing.
//
// A pass that admits nothing now defers the window's next expansion by a
// jittered exponential backoff, from DiscoveryExpandRetryMin doubling to
// DiscoveryExpandRetryMax. Any admission clears it, so a window that is filling
// keeps its cadence. Losing a working exit clears it, so a replacement is not
// held back by earlier failed spares. A network change clears it, since
// failures on the old path say nothing about the new one. The first pass is
// never deferred. The enumerator lists and mints nothing while the window is
// deferred, and the candidate it minted ahead of the next pass is held across
// the deferral rather than expiring unused and being replaced. Only expansion
// is paced: resize keeps its health, rotation and collapse cadence, and the
// window keeps serving the clients it has.
//
// The state is per window and guarded by the window stateLock.
package connect

import (
	"time"
	"weak"
)

// A discovery window's consecutive passes that admitted nothing, the earliest
// time it may expand again, and its exit count at the last pass.
type discoveryExpandRetry struct {
	failureCount int
	notBefore    time.Time
	clientCount  int
	// A weak exact source identity never retains its packet/flow graph.
	serviceUpdate  weak.Pointer[multiClientChannelUpdate]
	serviceLineage uint64
}

// Records an expansion pass that admitted no candidate and defers the next.
func (self *multiClientWindow) noteDiscoveryExpandUnproductive(now time.Time, serviceOwners ...*tcpSynRetry) {
	var serviceOwner *tcpSynRetry
	if len(serviceOwners) == 1 {
		serviceOwner = serviceOwners[0]
	}
	// bare fixture windows carry no settings and no pacing
	if self.settings == nil {
		return
	}
	retryMin := self.settings.DiscoveryExpandRetryMin
	retryMax := self.settings.DiscoveryExpandRetryMax
	if retryMin <= 0 {
		return
	}
	if retryMax < retryMin {
		retryMax = retryMin
	}
	if hook := self.beforeDiscoveryRetryPublishForTest; hook != nil {
		hook(serviceOwner)
	}
	var failureCount int
	var delay time.Duration
	recorded := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if serviceOwner != nil && self.serviceRefreshPassAnsweredWithLock(serviceOwner) {
			return false
		}
		self.discoveryRetry.serviceUpdate, self.discoveryRetry.serviceLineage = weak.Pointer[multiClientChannelUpdate]{}, 0
		if serviceOwner != nil && self.serviceRefreshOwner == serviceOwner && self.serviceRefreshAttempted && serviceOwner.update != nil {
			self.discoveryRetry.serviceUpdate = weak.Make(serviceOwner.update)
			self.discoveryRetry.serviceLineage = serviceOwner.retryLineage
		}
		self.discoveryRetry.failureCount += 1
		delay = self.retryJitter(fixedDestinationRetryDelay(self.discoveryRetry.failureCount, retryMin, retryMax))
		self.discoveryRetry.notBefore = now.Add(delay)
		if self.resizeMonitor != nil {
			self.resizeMonitor.NotifyAll()
		}
		failureCount = self.discoveryRetry.failureCount
		return true
	}()
	if !recorded {
		return
	}
	if self.log != nil {
		self.log.Infof("%s\n", relEvent(
			"discovery_expand_deferred",
			"failures", failureCount,
			"delay", delay,
		))
	}
}

// An admitted candidate shows the window is filling. Its next pass that admits
// nothing starts the backoff over.
func (self *multiClientWindow) noteDiscoveryExpandAdmitted() {
	self.clearDiscoveryExpandRetry()
}

// Records the window's exit count at the start of a pass. A drop means a
// working exit was lost, and its replacement is not held back by the passes
// that failed to add a spare before it. A retained warned exit lowers this
// count too, so publish the reset even though the old client remains owned.
func (self *multiClientWindow) noteDiscoveryClientCount(clientCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if clientCount < self.discoveryRetry.clientCount {
		deferred := !self.discoveryRetry.notBefore.IsZero()
		self.discoveryRetry = discoveryExpandRetry{}
		if deferred && self.resizeMonitor != nil {
			self.resizeMonitor.NotifyAll()
		}
	}
	self.discoveryRetry.clientCount = clientCount
}

// Ends any deferral and wakes its waiting producer under the same lock.
// Repeated clears preserve the current notification generation.
func (self *multiClientWindow) clearDiscoveryExpandRetry() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	deferred := !self.discoveryRetry.notBefore.IsZero()
	self.discoveryRetry = discoveryExpandRetry{
		clientCount: self.discoveryRetry.clientCount,
	}
	if deferred && self.resizeMonitor != nil {
		self.resizeMonitor.NotifyAll()
	}
}

// Whether a discovery window must not expand at `now`, and if so, when it may.
// Subscribe with the snapshot so a clear between the check and wait is visible.
func (self *multiClientWindow) discoveryExpandDeferral(now time.Time, changed ...*<-chan struct{}) (time.Time, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(changed) != 0 && changed[0] != nil {
		*changed[0] = nil
		if self.resizeMonitor != nil {
			*changed[0] = self.resizeMonitor.NotifyChannel()
		}
	}
	if !now.Before(self.discoveryRetry.notBefore) {
		return time.Time{}, false
	}
	return self.discoveryRetry.notBefore, true
}

// The enumerator's wait for a deferred discovery window. A clear, exit loss or
// network change ends it at once. A fixed window keeps its
// per-destination pacing instead. False when the window closed.
func (self *multiClientWindow) waitDiscoveryExpandDeferral() bool {
	if _, fixed := self.generator.FixedDestinationSize(); fixed {
		return true
	}
	// one timer for the wait, re-armed after each early wake
	var waitTimer *time.Timer
	defer func() {
		if waitTimer != nil {
			waitTimer.Stop()
		}
	}()
	for {
		// before the check, so a change that lands after it still ends the wait
		networkChanged := self.networkChangeNotify()
		var retryChanged <-chan struct{}
		notBefore, deferred := self.discoveryExpandDeferral(time.Now(), &retryChanged)
		if !deferred {
			return true
		}
		select {
		case <-self.ctx.Done():
			return false
		case <-networkChanged:
		case <-retryChanged:
		case <-resetOrCreateTimer(&waitTimer, time.Until(notBefore)):
		}
	}
}

// Offers a minted candidate to expansion until `expirationTime`. A candidate
// that would expire while its discovery window is deferred is held until one
// resize interval after the deferral ends instead: the enumerator mints one
// candidate ahead of the pass that takes it, and that identity has contacted
// no provider, so retiring it during the backoff only made the next expansion
// mint a replacement. False when the window closed or the candidate expired.
func (self *multiClientWindow) offerClientArgs(args *multiClientChannelArgs, expirationTime time.Time, serviceOwners ...*tcpSynRetry) bool {
	var serviceDone <-chan struct{}
	if len(serviceOwners) != 0 && serviceOwners[0] != nil {
		snapshot := self.serviceRefreshSnapshot()
		if snapshot.owner != serviceOwners[0] {
			return false
		}
		serviceDone = snapshot.update.ctx.Done()
	}
	// one timer for the offer, stopped as soon as the candidate is taken
	var expirationTimer *time.Timer
	defer func() {
		if expirationTimer != nil {
			expirationTimer.Stop()
		}
	}()
	for {
		select {
		case <-self.ctx.Done():
			return false
		case <-serviceDone:
			return false
		case self.clientChannelArgs <- args:
			return true
		case <-resetOrCreateTimer(&expirationTimer, time.Until(expirationTime)):
			notBefore, deferred := self.discoveryExpandDeferral(time.Now())
			if !deferred {
				return false
			}
			expirationTime = notBefore.Add(self.settings.WindowResizeTimeout)
		}
	}
}

// A useful reply clears only the failed attempted pass for this exact owned
// update and paid native lineage. Ordinary or future passes remain.
func (self *multiClientWindow) clearDiscoveryExpandRetryForService(update *multiClientChannelUpdate, lineage uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	retry := &self.discoveryRetry
	if update == nil || retry.serviceUpdate.Value() != update || lineage == 0 || retry.serviceLineage != lineage {
		return
	}
	deferred := !retry.notBefore.IsZero()
	self.discoveryRetry = discoveryExpandRetry{clientCount: retry.clientCount}
	if deferred && self.resizeMonitor != nil {
		self.resizeMonitor.NotifyAll()
	}
}
