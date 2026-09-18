// Common delivery prices a shared pacing clock without becoming a logical
// lane's window-sizing evidence or replacing physical serialization samples.
package connect

import "time"

// Finalize after retained admission and on every earlier return. Statistics
// read the same evidence without publishing a new hold or learning capacity.
func (self *SendSequence) finalizeWindowPacing(at time.Time, retain bool, generation time.Time, estimate *SendWindowEstimate) {
	service := self.windowPacer.service
	paced := service != nil && self.transferFlightPolicy().h1Only
	if paced {
		residence := estimate.WindowRoundTrip
		if residence <= 0 {
			residence = service.roundTripEvidence(at).residence
		}
		if residence > 0 {
			delivery := service.aggregateDelivery(at, residence, self.receiveWindowSetAtNanos.Load())
			if delivery.bytes-delivery.firstBytes >= min(estimate.Initial, kib(4)) {
				estimate.AggregateDeliveryByteRate = delivery.byteRate()
			}
		}
		estimate.PacingDiscovery, estimate.PacingHeldByteRate = service.pacingHold()
	}
	estimate.PacingByteRate = windowPacingRate(*estimate, estimate.PacingProbeByteRate)
	// A blind read has no local residence to hold a pace against. A sibling
	// reset cannot let an earlier estimate reinstate a retired generation.
	if paced && retain && estimate.WindowRoundTrip > 0 {
		service.holdPacingForGeneration(estimate.PacingByteRate, generation)
	}
}
