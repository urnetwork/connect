// The ACK snapshot may consume a withdrawal's wake after Run captured an
// earlier eviction cut. Keep that independent pending state level-triggered
// at the owner's final wait, without reapplying a cut against an old snapshot.
package connect

// Constant work under the leaf queue lock. Run's synchronous rewrites reattach
// before reaching its final wait. A detached-only boundary caller must wait
// for that owner transition, not repeatedly rephase an unavailable item.
func (self *SendSequence) pendingEvictionsRunnable() bool {
	queue := self.resendQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	return !self.detachedAck.active &&
		self.pendingEvictionPhases&self.pendingEvictionPhaseWithLock() != 0
}
