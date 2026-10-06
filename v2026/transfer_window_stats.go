// Destination diagnostics retain one complete window estimate and its evidence.
package connect

// The caller owns this statistics value. Equal windows preserve the existing
// preference for cumulative evidence, then keep the first equivalent estimate.
func (self *SendDestinationStats) observeWindowEstimate(estimate SendWindowEstimate) {
	if self.SendWindow.Window < estimate.Window ||
		(self.SendWindow.Window == estimate.Window && estimate.Sized && !self.SendWindow.Sized) {
		self.SendWindow = estimate
	}
}
