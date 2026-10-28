//go:build linux

package connect

import (
	"golang.org/x/sys/unix"
)

// CLOCK_BOOTTIME counts time spent in suspend, unlike the CLOCK_MONOTONIC Go's
// runtime reads (DELAY.md §5.2). This also covers android, which satisfies the
// linux build constraint.
func transportDelaySleepInclusiveNanos() (int64, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, false
	}
	return ts.Nano(), true
}
