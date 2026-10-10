//go:build windows

package connect

import (
	"time"
)

// The cpu time of the scale tests (GEOMAP §5.8 item 5), which this build reads
// as the wall clock.

// When the test binary started.
var testProcessStartTime = time.Now()

// The wall time since the test binary started, standing in for the process's
// cpu time.
func testProcessCpuTime() time.Duration {
	return time.Since(testProcessStartTime)
}
