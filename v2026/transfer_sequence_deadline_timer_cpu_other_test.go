//go:build !unix

package connect

import (
	"time"
)

// processCpu reads no CPU time where getrusage is unavailable, so the
// benchmark's cpu-ns/op is zero there.
func processCpu() time.Duration {
	return 0
}
