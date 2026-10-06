//go:build linux

// A fresh anonymous inode observes allocation, writing and durability without
// publishing a probe name or leaving cleanup work after a process interruption.
package durablevolume

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Each invocation owns one short-lived descriptor and one fixed-size write.
// Unsupported anonymous allocation refuses admission; there is no pathname
// fallback, success cache, whole-volume sync or background worker to outlive it.
func (self *Owner) writeHealth() (resultErr error) {
	// Package-owned host fixtures may refuse an operation but cannot replace
	// successful kernel I/O. Close is observed after releasing the descriptor.
	observe := func(operation string, file *os.File) error {
		if host, ok := self.host.(interface {
			observeWriteHealth(string, *os.File) error
		}); ok {
			return host.observeWriteHealth(operation, file)
		}
		return nil
	}
	if err := observe("open", self.rootFile); err != nil {
		return unavailableObservation("durable write probe could not be allocated", err)
	}
	fd, err := unix.Openat(int(self.rootFile.Fd()), ".", unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC|unix.O_EXCL, 0600)
	if err != nil {
		return unavailableObservation("durable write probe could not be allocated", err)
	}
	file := os.NewFile(uintptr(fd), "durable write probe")
	defer func() {
		closeErr := errors.Join(file.Close(), observe("file-close", file))
		syncErr := observe("root-sync", self.rootFile)
		if syncErr == nil {
			syncErr = self.rootFile.Sync()
		}
		resultErr = errors.Join(resultErr,
			unavailableObservation("durable write probe could not be closed", closeErr),
			unavailableObservation("durable root could not be synced", syncErr))
	}()
	writeErr := observe("file-write", file)
	if writeErr == nil {
		_, writeErr = file.Write([]byte("durable-volume-write-health\n"))
	}
	if writeErr != nil {
		return unavailableObservation("durable write probe could not be written", writeErr)
	}
	syncErr := observe("file-sync", file)
	if syncErr == nil {
		syncErr = file.Sync()
	}
	return unavailableObservation("durable write probe could not be synced", syncErr)
}
