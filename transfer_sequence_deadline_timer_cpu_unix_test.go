//go:build unix

package connect

import (
	"syscall"
	"time"
)

// processCpu is the process's user and system CPU time.
func processCpu() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
