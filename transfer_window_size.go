// Window capacity and pacing have separate owners: valid delivery may grow
// retained capacity, while the service sampler continues to vary the rate.
package connect

import (
	"sync"
	"time"
)

// A leaf lock protects admission's learned value from statistics readers.
// Only fresh evidence in an explicitly requested remeasurement may shrink it.
type sendWindowSizeState struct {
	stateLock      sync.Mutex
	window         ByteCount
	initialized    bool
	remeasureUntil time.Time
}

// Only the sending worker commits evidence. Hard memory and receiver limits
// clamp the returned value at the caller without erasing learned capacity.
func (self *sendWindowSizeState) estimate(initial, candidate ByteCount, qualified, retain bool) ByteCount {
	return self.estimateAt(initial, candidate, qualified, retain, time.Time{}, false)
}

// Notification itself grants no bytes and teaches no smaller window.
func (self *sendWindowSizeState) remeasure(at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.remeasureUntil = at.Add(windowQualityRemeasureInterval)
}

// Statistics cannot commit a shrink; both rate and RTT must belong to this
// generation, and the caller serializes reset against this admission read.
func (self *sendWindowSizeState) estimateAt(initial, candidate ByteCount, qualified, retain bool, at time.Time, fresh bool) ByteCount {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	window := self.window
	if !self.initialized {
		window = initial
	}
	if retain {
		if qualified {
			if fresh && !at.IsZero() && at.Before(self.remeasureUntil) {
				window = candidate
			} else {
				window = max(window, candidate)
			}
		}
		self.window, self.initialized = window, true
	}
	return window
}
