//go:build darwin

// Darwin has no anonymous inode allocation. The probe is created beside the
// owner's external lease, which declaration validation keeps outside every
// journal root on the same verified filesystem, and its random name is removed
// before any byte is written. An interruption can therefore leave at most an
// empty probe name in the lease directory: never inside a custody inventory
// and never in place of an existing name.
package durablevolume

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Each invocation owns one short-lived descriptor and one fixed-size write.
// There is no success cache, whole-volume sync or background worker to outlive
// it; the probe directory and root are both synced before admission.
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
	directory, err := openPhysicalDirectory(filepath.Dir(self.rootSpec.LeasePath))
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, unavailableObservation("durable write probe directory could not be closed", directory.Close()))
	}()
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return unavailableObservation("durable write probe directory could not be observed", err)
	}
	if statDevice(&stat) != self.mount.Device {
		return errors.Join(ErrIdentity, errors.New("durable write probe directory belongs to another filesystem"))
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return unavailableObservation("durable write probe name could not be chosen", err)
	}
	name := ".urnetwork-write-health-" + hex.EncodeToString(nonce)
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return unavailableObservation("durable write probe could not be allocated", err)
	}
	file := os.NewFile(uintptr(fd), "durable write probe")
	unlinkErr := unix.Unlinkat(int(directory.Fd()), name, 0)
	defer func() {
		closeErr := errors.Join(file.Close(), observe("file-close", file))
		nameErr := directory.Sync()
		syncErr := observe("root-sync", self.rootFile)
		if syncErr == nil {
			syncErr = self.rootFile.Sync()
		}
		resultErr = errors.Join(resultErr,
			unavailableObservation("durable write probe could not be closed", closeErr),
			unavailableObservation("durable write probe directory could not be synced", nameErr),
			unavailableObservation("durable root could not be synced", syncErr))
	}()
	if unlinkErr != nil {
		return unavailableObservation("durable write probe name could not be removed", unlinkErr)
	}
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
