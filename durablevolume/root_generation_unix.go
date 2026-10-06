//go:build linux || darwin

// An externally bound inode and nonce authenticate state roots across restarts.
// Admission reads only: provisioning and restore rebinding are separate acts.
package durablevolume

import (
	"github.com/urnetwork/connect/durablesys"
	"golang.org/x/sys/unix"

	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"runtime"
	"syscall"
)

// Exact descriptor-relative xattrs cannot follow a substituted pathname.
func readRootGeneration(file *os.File) ([]byte, error) {
	if file == nil {
		return nil, ErrClosed
	}
	raw := make([]byte, RootGenerationBytes+1)
	count, err := durablesys.GetAttribute(int(file.Fd()), RootGenerationAttribute, raw)
	runtime.KeepAlive(file)
	if err != nil {
		return nil, generationObservation(err)
	}
	if count != RootGenerationBytes {
		return nil, errors.Join(ErrIdentity, errors.New("durable root generation has another length"))
	}
	return raw[:RootGenerationBytes], nil
}

// Missing/malformed authority is proven loss; unsupported/failed probes are not.
func generationObservation(err error) error {
	if errors.Is(err, durablesys.ErrNoAttribute) || errors.Is(err, syscall.ERANGE) {
		return errors.Join(ErrIdentity, err)
	}
	if durablesys.AttributeUnsupported(err) {
		return errors.Join(ErrUnsupported, unavailableObservation("durable root generation cannot be observed on this filesystem", err))
	}
	return unavailableObservation("durable root generation could not be observed", err)
}

// Recycled inode numbers do not inherit a former root's nonce authority.
func (self *Owner) rootGeneration() ([]byte, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(self.rootFile.Fd()), &stat); err != nil {
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
