// Physical flight can prove that paced service is already draining a queue.
// A compulsory empty-flight probe would add one propagation turn of idle;
// reserve it for excess residence whose queue is no longer making progress.
package connect

import "time"

// Compare complete feedback turns, allowing one permitted physical burst of
// variation. A falling count grants only the next turn; a stall or increase
// expires it without changing the existing probe cooldown or absolute limit.
func (self *windowPacingService) observeDrainProgressWithLock(now time.Time, interval time.Duration) bool {
	flight := self.outstandingWithLock()
	if self.pendingWrites == 0 || self.drainObservedAt.IsZero() || now.Before(self.drainObservedAt) {
		self.drainObservedAt, self.drainObservedFlight = now, flight
		self.drainProgressUntil = time.Time{}
		return false
	}
	if now.Sub(self.drainObservedAt) >= interval {
		allowance := float64(max(self.maxMessageByteCount, self.burstMeter.limit))
		if self.drainObservedFlight-flight > allowance {
			self.drainProgressUntil = now.Add(interval)
		}
		self.drainObservedAt, self.drainObservedFlight = now, flight
	}
	return now.Before(self.drainProgressUntil)
}
