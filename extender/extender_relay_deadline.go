package extender

import (
	"time"
)

// A read or write deadline of one relay direction, re-armed lazily (CPUOPT2
// §9.2 finding 1). A relay used to install a fresh deadline before every 4 KiB
// chunk, in both directions, and each install is a timer change in the runtime
// poller. Here an operation keeps the installed deadline while it still has
// the timeout left. With less left, the deadline is re-armed at the timeout
// plus the slack, so the operations that follow in a busy direction keep it
// for about one slack. A deadline that has passed, or the first one, is armed
// at the timeout alone: there is no burst yet to spread it over, so a slack
// would only delay noticing a peer that stopped.
//
// Every operation therefore still fails no earlier than the timeout after it
// starts, as when each chunk installed its own deadline, and at most the slack
// later.
//
// One goroutine owns each value.
type relayDeadline struct {
	timeout time.Duration
	slack   time.Duration
	// zero until the first operation
	deadline time.Time
}

// The slack is slackFraction of the timeout. A non-positive fraction re-arms
// before every operation, as a relay did before.
func newRelayDeadline(timeout time.Duration, slackFraction float64) relayDeadline {
	var slack time.Duration
	if 0 < slackFraction {
		slack = time.Duration(float64(timeout) * slackFraction)
	}
	return relayDeadline{
		timeout: timeout,
		slack:   slack,
	}
}

// The deadline for an operation that starts at now, and whether it changed and
// must be installed before the operation.
func (self *relayDeadline) next(now time.Time) (time.Time, bool) {
	if self.deadline.IsZero() || !now.Before(self.deadline) {
		self.deadline = now.Add(self.timeout)
		return self.deadline, true
	}
	if self.deadline.Sub(now) < self.timeout {
		self.deadline = now.Add(self.timeout + self.slack)
		return self.deadline, true
	}
	return self.deadline, false
}
