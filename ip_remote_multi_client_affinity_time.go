// Affinity age follows the original connection creation across new flows and DNS
// hints. It never changes an existing tuple's connection ownership.
package connect

import (
	"sync/atomic"
	"time"
)

// Immutable after publication. Retains one provider and monotonic timestamp,
// not an ancestor update, its context, race buffers or donor-index maps.
type multiClientAffinitySelection struct {
	client     *multiClientChannel
	createTime time.Time
}

// One atomic pointer publishes provider and provenance together. originTime
// starts at update creation and can only be lowered by INITIAL inheritance,
// before sendUpdate releases the parent lock or exposes the new update to a
// leaf/race owner. It is immutable thereafter, including through nil unbind.
// This scalar retains no provider/update root. Like atomic.Pointer, a binding
// must not be copied after use.
type multiClientAffinityBinding struct {
	selection  atomic.Pointer[multiClientAffinitySelection]
	originTime time.Time
}

// The selected provider, retaining the existing atomic client API.
func (self *multiClientAffinityBinding) Load() *multiClientChannel {
	if selection := self.selection.Load(); selection != nil {
		return selection.client
	}
	return nil
}

// No bookkeeping or rebind can renew the creating update's original age.
func (self *multiClientAffinityBinding) Store(client *multiClientChannel) {
	if client == nil {
		self.selection.Store(nil)
		return
	}
	var next *multiClientAffinitySelection
	for {
		previous := self.selection.Load()
		if previous != nil && previous.client == client {
			return
		}
		if next == nil {
			next = &multiClientAffinitySelection{client: client, createTime: self.originTime}
		}
		if self.selection.CompareAndSwap(previous, next) {
			return
		}
	}
}

// Preserves pointer-based compare-and-swap semantics for compatibility. An
// operation holding a specific selection receipt instead uses the exact
// receipt CAS below so A->B->A cannot erase a newer selection of A.
func (self *multiClientAffinityBinding) CompareAndSwap(old, next *multiClientChannel) bool {
	var replacement *multiClientAffinitySelection
	for {
		previous := self.selection.Load()
		var current *multiClientChannel
		if previous != nil {
			current = previous.client
		}
		if current != old {
			return false
		}
		if old == next {
			return true
		}
		if next != nil && replacement == nil {
			replacement = &multiClientAffinitySelection{client: next, createTime: self.originTime}
		}
		if self.selection.CompareAndSwap(previous, replacement) {
			return true
		}
	}
}

// A rejected send owns only the binding it actually attempted, not a later
// rebind or a new provider race that happens to choose the same channel.
func (self *multiClientAffinityBinding) clearSelection(selection *multiClientAffinitySelection) bool {
	return self.selection.CompareAndSwap(selection, nil)
}

// Returns provider and immutable origin from the same atomic publication.
func (self *multiClientAffinityBinding) loadSelection() *multiClientAffinitySelection {
	return self.selection.Load()
}

// Missing or mismatched provenance fails open to normal provider racing.
// A nonpositive cap disables inheritance; equality is already expired.
func (self *multiClientAffinitySelection) eligible(client *multiClientChannel, now time.Time, maxTime time.Duration) bool {
	return self != nil && client != nil && self.client == client && maxTime > 0 &&
		!self.createTime.IsZero() && !now.Before(self.createTime) && now.Sub(self.createTime) < maxTime
}

// A new provider choice gets a distinct receipt, but keeps this update's
// creation/lineage age even if setup took time or a race chose the same client.
// Only a genuinely new update can start a newer age. One atomic publication
// serves parent-owned rebinds and leaf-owned winners without nested locks.
func (self *multiClientChannelUpdate) selectAffinityClient(client *multiClientChannel) {
	if client == nil {
		self.client.Store(nil)
		return
	}
	self.client.selection.Store(&multiClientAffinitySelection{client: client, createTime: self.client.originTime})
}

// INITIAL new-flow inheritance only: all production callers are inside the
// newly constructed branch of sendUpdate, under its parent lock. DNS, ordinary
// group, late-name bridge and app-canonical pin placement run before that lock
// releases; no leaf/race owner can yet access this update. Rebind, runtime
// index rebuild and established tuple paths MUST NOT call this method.
// The conservative minimum of this creation time and inherited lineage time
// becomes immutable before the receipt is published. Unknown pin provenance
// stays unknown across later nil/unbind/rebind; it cannot fabricate fresh age.
func (self *multiClientChannelUpdate) inheritAffinityClient(client *multiClientChannel, selection *multiClientAffinitySelection) {
	if client == nil {
		self.client.Store(nil)
		return
	}
	origin := self.client.originTime
	if selection == nil || selection.client != client || selection.createTime.IsZero() || time.Now().Before(selection.createTime) {
		origin = time.Time{}
	} else if !origin.IsZero() && selection.createTime.Before(origin) {
		origin = selection.createTime
	}
	self.client.originTime = origin
	if selection == nil || selection.client != client || !selection.createTime.Equal(origin) {
		selection = &multiClientAffinitySelection{client: client, createTime: origin}
	}
	self.client.selection.Store(selection)
}

// A preliminary ranking snapshot, not authority to adopt. The final channel
// lock below rechecks health after route lookup and immediately before store.
func (self *multiClientChannel) positiveAffinityHealth() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.healthy && self.endErr == nil
}

// The parent routing lock is held. Carrier availability is a separate fresh
// snapshot, taken before the channel lock to preserve RouteManager lock order.
// Warning, health and terminal-error state serialize with the actual store;
// a later lifecycle change keeps the existing established-flow policy.
func (self *RemoteUserNatMultiClient) adoptAffinityClientWithLock(
	update *multiClientChannelUpdate,
	client *multiClientChannel,
	selection *multiClientAffinitySelection,
) bool {
	if client == nil {
		return false
	}
	if self.beforeAffinityAdoptionForTest != nil {
		self.beforeAffinityAdoptionForTest(client)
	}
	if !client.hasActiveTransport() {
		return false
	}
	client.stateLock.Lock()
	defer client.stateLock.Unlock()
	if client.IsDone() || !client.healthy || client.endErr != nil || client.warning || client.quarantined ||
		update.ipPath != nil && !client.supportsIpVersion(update.ipPath.Version) {
		return false
	}
	select {
	case <-client.clientDone():
		return false
	default:
	}
	if !update.pinned {
		now := time.Now()
		maxTime := self.reliabilitySettings().AffinityMaxTime
		origin := update.client.originTime
		if !selection.eligible(client, now, maxTime) ||
			origin.IsZero() || now.Before(origin) || now.Sub(origin) >= maxTime {
			return false
		}
		if self.affinityDonorServiceFailedWithLock(update, client, now) {
			return false
		}
	}
	update.inheritAffinityClient(client, selection)
	return true
}
