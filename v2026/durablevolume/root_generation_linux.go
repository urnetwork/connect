//go:build linux

// An externally bound inode and nonce authenticate state roots across restarts.
// Admission reads only: provisioning and restore rebinding are separate acts.
package durablevolume

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// Exact descriptor-relative xattrs cannot follow a substituted pathname.
func readRootGeneration(file *os.File) ([]byte, error) {
	if file == nil {
		return nil, ErrClosed
	}
	name, err := syscall.BytePtrFromString(RootGenerationAttribute)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, RootGenerationBytes+1)
	count, _, errno := syscall.Syscall6(syscall.SYS_FGETXATTR, file.Fd(), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)), 0, 0)
	runtime.KeepAlive(file)
	if errno != 0 {
		return nil, generationObservation(errno)
	}
	if count != RootGenerationBytes {
		return nil, errors.Join(ErrIdentity, errors.New("durable root generation has another length"))
	}
	return raw[:RootGenerationBytes], nil
}

// Missing/malformed authority is proven loss; unsupported/failed probes are not.
func generationObservation(err error) error {
	if errors.Is(err, syscall.ENODATA) || errors.Is(err, syscall.ERANGE) {
		return errors.Join(ErrIdentity, err)
	}
	if errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) {
		return errors.Join(ErrUnsupported, unavailableObservation("durable root generation cannot be observed on this filesystem", err))
	}
	return unavailableObservation("durable root generation could not be observed", err)
}

// Recycled inode numbers do not inherit a former root's nonce authority.
func (self *Owner) rootGeneration() ([]byte, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(self.rootFile.Fd()), &stat); err != nil {
		return nil, unavailableObservation("durable root inode could not be observed", err)
	}
	if stat.Ino != self.rootSpec.RootInode {
		return nil, errors.Join(ErrIdentity, errors.New("durable root differs from its preprovisioned inode"))
	}
	if self.observeFile != nil {
		if err := self.observeFile("root-generation", self.rootFile, self.rootPath); err != nil {
			return nil, generationObservation(err)
		}
	}
	raw, err := readRootGeneration(self.rootFile)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(digest[:]) != self.rootSpec.GenerationSha256 {
		return nil, errors.Join(ErrIdentity, errors.New("durable root generation differs from its external authority"))
	}
	return raw, nil
}
