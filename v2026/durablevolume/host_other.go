//go:build !linux && !darwin

// Unsupported hosts cannot claim the Linux volume and descriptor contract.
package durablevolume

import (
	"context"
	"errors"
	"os"
)

// No fallback treats ordinary directories as approved durable mounts.
func defaultHost() Host { return nil }

// Declaration reads cannot bypass the platform contract.
func readProtectedFile(string, int) ([]byte, error) { return nil, ErrUnsupported }

// Cancellation remains a pre-I/O refusal on unsupported hosts too.
func readProtectedFileContext(ctx context.Context, _ string, _ int) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("durable inventory context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

// Construction never silently selects a weaker filesystem implementation.
func (self *Owner) open() error { return ErrUnsupported }

// Unsupported owners cannot produce successful admission.
func (self *Owner) check(bool) error { return ErrUnsupported }

// No successful write-health observation exists on unsupported hosts.
func (self *Owner) writeHealth() error { return ErrUnsupported }

// No descendant descriptor is exposed without the platform contract.
func (self *Owner) openDirectory(string, bool) (*os.File, error) { return nil, ErrUnsupported }

// Unsupported descriptor custody remains explicit.
func (self *Owner) checkDirectory(string, *os.File, bool) error { return ErrUnsupported }
