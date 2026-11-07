// One receive pump prepays transient ciphertext/plaintext and wrapper overlap.
// This is unused decode capacity between calls, distinct from live item claims.
// A known carrier ceiling is prepaid when it fits. Route changes cannot revoke
// an already-covered current frame; larger envelopes require actual growth.
// Custom routes retain observed-size admission, not an invented maximum or a
// universal promised-envelope guarantee.
package connect

// Construction and then the sole receive pump own growth; snapshots are atomic.
// Failure is ordinary pressure, never an authentication error or resident close.
func (self *Client) prepareReceiveDecodeMemory(carrier []byte, wireMaximum int) bool {
	settings := self.settings.ReceiveBufferSettings
	if !settings.ownsDecodedReceiveMemory() || self.settings.EncryptionSettings == nil ||
		self.settings.EncryptionSettings.Mode == EncryptionModeOff {
		return true
	}
	actualRootBytes := MessagePoolRootByteCount(carrier)
	rootBytes := actualRootBytes
	if wireMaximum > 0 {
		rootBytes = max(rootBytes, receiveMemoryScanCopyRoot(wireMaximum))
	}
	// Each decrypt attempt returns its failed destination before trying another
	// cipher. Two complete roots plus two rounded inline wrappers cover the
	// only simultaneous outer/inner overlap; no queued owner borrows this slot.
	required := addReceiveQueueByteCount(addReceiveQueueByteCount(rootBytes, rootBytes), 2048)
	// Scratch is a separate parent-owned capacity reservation, not encoded
	// Pack permission and not part of the RX item's configured leaf ceiling.
	if self.receiveDecodeMemoryBudget == nil {
		self.receiveDecodeMemoryBudget = settings.ReceiveQueueBudget
		if parent := settings.ReceiveQueueBudget.Parent(); parent != nil {
			self.receiveDecodeMemoryBudget = parent
		}
	}
	previous := self.receiveDecodeReservedByteCount.Load()
	if required <= previous {
		return true
	}
	if self.receiveDecodeMemoryBudget.TryReserve(required - previous) {
		self.receiveDecodeReservedByteCount.Store(required)
		return true
	}
	// A new route may advertise a larger ceiling while the actual head or
	// control still fits our paid slot. Ceiling growth is opportunistic; it
	// cannot withdraw a live generation's existing decode permission.
	actualRequired := addReceiveQueueByteCount(addReceiveQueueByteCount(actualRootBytes, actualRootBytes), 2048)
	if actualRequired <= previous {
		return true
	}
	if !self.receiveDecodeMemoryBudget.TryReserve(actualRequired - previous) {
		return false
	}
	self.receiveDecodeReservedByteCount.Store(actualRequired)
	return true
}

// Runs only after the synchronous pump has returned every local decrypt root.
// Queued/held owners keep their separate exact physical claims until return.
func (self *Client) releaseReceiveDecodeMemory() {
	if reserved := self.receiveDecodeReservedByteCount.Swap(0); reserved != 0 {
		self.receiveDecodeMemoryBudget.Release(reserved)
	}
}
