// RTT remeasurement preserves the provisional recovery estimate until a new
// path's exact first-write/ACK pair replaces the retired history.
package connect

import "time"

func (self *RttWindow) networkQualityChanged(at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.qualityAfterNanos = at.UnixNano()
	self.qualityPending = true
}

// A retained old estimate is useful for recovery, never fresh sizing proof.
func (self *RttWindow) freshQualityEstimate() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.qualityAfterNanos != 0 && !self.qualityPending
}
