//go:build darwin

package connect

import (
	"golang.org/x/sys/unix"
)

// Darwin's CLOCK_MONOTONIC keeps counting while the system sleeps, unlike the
// mach_absolute_time clock Go's runtime reads (DELAY.md §5.2). This also
// covers ios, which satisfies the darwin build constraint.
func transportDelaySleepInclusiveNanos() (int64, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, false
	}
	return ts.Nano(), true
}
