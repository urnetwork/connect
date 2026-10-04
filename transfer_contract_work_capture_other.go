//go:build !linux && !darwin && !freebsd

// Unsupported descriptor custody remains explicit evidence unavailability.
package connect

import (
	"errors"
	"os"
)

// Ordinary transfer remains available without a supported durable cut owner.
func lockOriginalWorkOutbox(*os.Root) (*os.File, error) {
	return nil, errors.New("whole-work durable capture is unavailable on this platform")
}
