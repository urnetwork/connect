// Window capacity and pacing have separate owners: valid delivery may grow
// retained capacity, while the service sampler continues to vary the rate.
package connect

import "sync"

// A leaf lock protects admission's learned value from statistics readers.
// Network-quality remeasurement is intentionally not wired into this first
// policy comparison; ordinary feedback can only grow the learned capacity.
type sendWindowSizeState struct {
	stateLock   sync.Mutex
	window      ByteCount
	initialized bool
}

// Only the sending worker commits evidence. Hard memory and receiver limits
// clamp the returned value at the caller without erasing learned capacity.
func (self *sendWindowSizeState) estimate(initial, candidate ByteCount, qualified, retain bool) ByteCount {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	window := self.window
	if !self.initialized {
		window = initial
	}
	if retain {
		if qualified {
			window = max(window, candidate)
		}
		self.window, self.initialized = window, true
	}
	return window
}
