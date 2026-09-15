package connect

import (
	"context"
	"time"
)

// H1 terminates at a relay; its socket can accept a whole deep window before
// the forwarding queue serializes it. Limit bursts to two milliseconds of the
// configured target wire rate. The window still owns flight and memory limits.
// One sequence owns this pacer, including recovery writes, and waits only once
// per burst. Idle time never accumulates permission for a window-sized burst.
type windowBurstPacer struct {
	next  time.Time
	timer *time.Timer
}

func (self *windowBurstPacer) wait(ctx context.Context, byteCount int, rate ByteCount) error {
	now := time.Now()
	if self.next.Before(now) {
		self.next = now
	}
	self.next = self.next.Add(time.Duration(float64(byteCount) * float64(time.Second) / float64(rate)))
	if delay := self.next.Sub(now); delay > 2*time.Millisecond {
		if self.timer == nil {
			self.timer = time.NewTimer(0)
		}
		self.timer.Reset(delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-self.timer.C:
		}
	}
	return nil
}

func (self *windowBurstPacer) close() {
	if self.timer != nil {
		self.timer.Stop()
	}
}
