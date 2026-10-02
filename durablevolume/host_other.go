//go:build !linux

// Unsupported hosts cannot claim the Linux volume and descriptor contract.
package durablevolume

import "os"

// No fallback treats ordinary directories as approved durable mounts.
func defaultHost() Host { return nil }

// Declaration reads cannot bypass the platform contract.
func readProtectedFile(string, int) ([]byte, error) { return nil, ErrUnsupported }

// Construction never silently selects a weaker filesystem implementation.
func (self *Owner) open() error { return ErrUnsupported }

// Unsupported owners cannot produce successful admission.
func (self *Owner) check(bool) error { return ErrUnsupported }

// No descendant descriptor is exposed without the platform contract.
func (self *Owner) openDirectory(string, bool) (*os.File, error) { return nil, ErrUnsupported }

// Unsupported descriptor custody remains explicit.
func (self *Owner) checkDirectory(string, *os.File) error { return ErrUnsupported }
