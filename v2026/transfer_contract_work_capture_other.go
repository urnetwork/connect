//go:build !linux && !darwin && !freebsd

// Unsupported descriptor custody remains explicit evidence unavailability.
package connect

import (
	"context"
	"errors"
	"os"
)

// Ordinary transfer remains available without a supported durable cut owner.
func lockOriginalWorkOutbox(*os.Root) (*os.File, error) {
	return nil, errors.New("whole-work durable capture is unavailable on this platform")
}

var errOriginalWorkOutboxAttributeAbsent = errors.New("whole-work outbox birth attribute is absent")

func originalWorkOutboxNoFollow() int                               { return 0 }
func originalWorkOutboxInode(os.FileInfo) uint64                    { return 0 }
func originalWorkOutboxDevice(os.FileInfo) uint64                   { return 0 }
func originalWorkOutboxPrivate(os.FileInfo, bool, os.FileMode) bool { return false }
func originalWorkOutboxSameOwner(os.FileInfo, os.FileInfo) bool     { return false }
func originalWorkOutboxSameState(os.FileInfo, os.FileInfo) bool     { return false }
func readOriginalWorkOutboxAttribute(*os.File) ([]byte, error) {
	return nil, errors.New("whole-work durable capture is unavailable on this platform")
}
func replaceOriginalWorkOutboxAttribute(*os.File, []byte, bool) error {
	return errors.New("whole-work durable capture is unavailable on this platform")
}

func BuildFreshOriginalWorkOutboxCheckpoint(context.Context, *os.File) ([]byte, error) {
	return nil, errors.New("whole-work durable preparation is unavailable on this platform")
}
