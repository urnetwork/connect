package connect

import (
	"time"
)

// FIXME when the server ping matches the client ping, we can increase this to 10s
const WakeupEpoch = 1 * time.Second

func WakeupAfter(d time.Duration) <-chan time.Time {
	now := time.Now()
	t := WakeupTime(now.Add(d))
	return time.After(t.Sub(now))
}

func WakeupTime(t time.Time) time.Time {
	r := (time.Duration(t.UnixNano()) * time.Nanosecond) % WakeupEpoch
	if r == 0 {
		return t
	}
	return t.Add(WakeupEpoch - r)
}
