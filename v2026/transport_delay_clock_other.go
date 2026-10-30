//go:build !darwin && !linux

package connect

// Without a known sleep-inclusive clock the transport clock is Go's monotonic
// clock alone (DELAY.md §5.2: verify the windows clock on a device).
func transportDelaySleepInclusiveNanos() (int64, bool) {
	return 0, false
}
